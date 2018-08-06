package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/Shopify/sarama"
	"github.com/golang/protobuf/ptypes"
	"github.com/open-horizon/examples/edge/msghub/sdr2msghub/audiolib"
	rtlsdr "github.com/open-horizon/examples/edge/services/sdr/rtlsdrclientlib"
	tf "github.com/tensorflow/tensorflow/tensorflow/go"
)

func opIsSafe(a string) bool {
	safeOPtypes := []string{
		"Const",
		"Placeholder",
		"Conv2D",
		"Cast",
		"Div",
		"StatelessRandomNormal",
		"ExpandDims",
		"AudioSpectrogram",
		"DecodeRaw",
		"Reshape",
		"MatMul",
		"Sum",
		"Softmax",
		"Squeeze",
		"RandomUniform",
		"Identity",
	}
	for _, b := range safeOPtypes {
		if b == a {
			return true
		}
	}
	return false
}

// model holds the session, the input placeholder and output.
type model struct {
	Sess    *tf.Session
	InputPH tf.Output
	Output  tf.Output
}

// goodness takes a chunk of raw audio with no headers and returns a value between 0 and 1.
// 1 for good (in this case speech), 0 for nongood (in this case nonspeech).
// the audio must be exactly 32 seconds long.
func (m *model) goodness(audio []byte) (value float32, err error) {
	// first we must convert the audio to a string tensor.
	inputTensor, err := tf.NewTensor(string(audio))
	if err != nil {
		return
	}
	// then feed the input into the input placeholder while pulling on the output.
	result, err := m.Sess.Run(map[tf.Output]*tf.Tensor{m.InputPH: inputTensor}, []tf.Output{m.Output}, nil)
	if err != nil {
		return
	}
	value = result[0].Value().([]float32)[0]
	return
}

func newModel(path string) (m model, err error) {
	def, err := ioutil.ReadFile(path)
	if err != nil {
		panic(err)
	}
	graph := tf.NewGraph()
	err = graph.Import(def, "")
	if err != nil {
		panic(err)
	}
	ops := graph.Operations()
	unsafeOPs := map[string]bool{}
	graphIsUnsafe := false
	for _, op := range ops {
		if !opIsSafe(op.Type()) {
			unsafeOPs[op.Type()] = true
			graphIsUnsafe = true
		}
	}
	if graphIsUnsafe {
		fmt.Println("The following OP types are not in whitelist:")
		for op := range unsafeOPs {
			fmt.Println(op)
		}
		err = errors.New("unsafe OPs")
		return
	}
	outputOP := graph.Operation("output")
	if outputOP == nil {
		err = errors.New("output OP not found")
		return
	}
	m.Output = outputOP.Output(0)

	inputPHOP := graph.Operation("input/Placeholder")
	if inputPHOP == nil {
		err = errors.New("input OP not found")
		return
	}
	m.InputPH = inputPHOP.Output(0)
	m.Sess, err = tf.NewSession(graph, nil)
	return
}

type msghubConn struct {
	Producer sarama.SyncProducer
	Topic    string
}

// taken from cloud/sdr/data-ingest/example-go-clients/util/util.go
func populateConfig(config *sarama.Config, user, pw, apiKey string) error {
	config.ClientID = apiKey
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.Retry.Max = 5
	config.Producer.Return.Successes = true
	config.Net.TLS.Enable = true
	config.Net.SASL.User = user
	config.Net.SASL.Password = pw
	config.Net.SASL.Enable = true
	return nil
}

func connect(topic string) (conn msghubConn, err error) {
	conn.Topic = topic
	apiKey := getEnv("MSGHUB_API_KEY")
	fmt.Println("msghub key:", apiKey)
	username := apiKey[:16]
	password := apiKey[16:]
	brokerStr := getEnv("MSGHUB_BROKER_URL")
	fmt.Println("url:", brokerStr)
	brokers := strings.Split(brokerStr, ",")
	config := sarama.NewConfig()
	err = populateConfig(config, username, password, apiKey)
	if err != nil {
		return
	}
	fmt.Println("now connecting to msghub")
	conn.Producer, err = sarama.NewSyncProducer(brokers, config)
	fmt.Println("done trying to connect")
	if err != nil {
		return
	}
	return
}

func (conn *msghubConn) publishAudio(audioMsg *audiolib.AudioMsg) (err error) {
	// as AudioMsg implements the sarama.Encoder interface, we can pass it directly to ProducerMessage.
	msg := &sarama.ProducerMessage{Topic: conn.Topic, Key: nil, Value: audioMsg}
	partition, offset, err := conn.Producer.SendMessage(msg)
	if err != nil {
		log.Printf("FAILED to send message: %s\n", err)
	} else {
		log.Printf("> message sent to partition %d at offset %d\n", partition, offset)
	}
	return
}

// read env vars from system with fall back.
func getEnv(keys ...string) (val string) {
	if len(keys) == 0 {
		panic("must give at least one key")
	}
	for _, key := range keys {
		val = os.Getenv(key)
		if val != "" {
			return
		}
	}
	if val == "" {
		fmt.Println("none of", keys, "are set")
		panic("can't any find set value")
	}
	return
}

// the default hostname if not overridden
var hostname string = "sdr"

func main() {
	alt_addr := os.Getenv("RTLSDR_ADDR")
	// if no alternative address is set, use the default.
	if alt_addr != "" {
		fmt.Println("connecting to remote rtlsdr:", alt_addr)
		hostname = alt_addr
	}
	devID := getEnv("HZN_ORG_ID") + "/" + getEnv("HZN_DEVICE_ID")
	// load the graph def from FS
	m, err := newModel("model.pb")
	if err != nil {
		panic(err)
	}
	fmt.Println("model loaded")
	topic := getEnv("MSGHUB_TOPIC")
	fmt.Printf("using topic %s\n", topic)
	conn, err := connect(topic)
	if err != nil {
		panic(err)
	}
	fmt.Println("connected to msghub")
	// create a map to hold the goodness for each station we have ever oberved.
	// This map will grow as long as the program lives
	stationGoodness := map[float32]float32{}
	lastStationsRefresh := time.Time{}
	for {
		// if it has been over 5 minuts since we last updated the list of strong stations,
		if time.Now().Sub(lastStationsRefresh) > (5 * time.Minute) {
			// for ever, we aquire a list of stations,
			stations, err := rtlsdr.GetCeilingSignals(hostname, -8)
			if err != nil {
				panic(err)
			}
			for _, station := range stations {
				_, prs := stationGoodness[station]
				if !prs {
					// only if the station is not already in our map, do we add it, with an initial value of 0.5
					fmt.Println("found new station: ", station)
					stationGoodness[station] = 0.5
				}
			}
			// if no stations can be found, we can't do anything, so panic.
			if len(stationGoodness) < 1 {
				panic("No FM stations. Move the antenna?")
			}
			fmt.Println("found", len(stations), "stations")
			fmt.Println(stationGoodness)
			lastStationsRefresh = time.Now()
		}
		for station, goodness := range stationGoodness {
			// if our goodness is less then a random number between 0 and 1.
			if rand.Float32() < goodness {
				audio, err := rtlsdr.GetAudio(hostname, int(station))
				if err != nil {
					panic(err)
				}
				val, err := m.goodness(audio)
				if err != nil {
					panic(err)
				}
				// if the value is close to 1, the goodness of that station will increase, if the value is small, the goodness will decrease.
				stationGoodness[station] = stationGoodness[station]*(val+0.3) + 0.05
				fmt.Println(station, "observed value:", val, "updated goodness:", stationGoodness[station])
				// if the value is over 0.5, it is worth sending to the cloud.
				if val > 0.5 {
					// construct the message,
					msg := &audiolib.AudioMsg{
						Audio:         audio,
						Ts:            ptypes.TimestampNow(),
						Freq:          station,
						ExpectedValue: val,
						DevID:         devID,
					}
					// and publish it to msghub
					err = conn.publishAudio(msg)
					if err != nil {
						fmt.Println(err)
					}
				}
			}
		}
	}
}
