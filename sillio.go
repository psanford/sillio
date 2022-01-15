package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/psanford/gogsm"
)

var modem = flag.String("modem", "/dev/ttyUSB1", "Modem device")
var postURL = flag.String("url", "", "URL to post messages to")
var username = flag.String("username", "", "Username")
var password = flag.String("password", "", "Username")
var slackToken = flag.String("slack-token", "", "Slack bot token")
var slackChannelID = flag.String("slack-channel", "", "Slack channel id")

func main() {

	flag.Parse()
	m, err := gogsm.NewModem(*modem)
	if err != nil {
		log.Fatal(err)
	}

	connectDone := make(chan struct{})
	go func() {
		select {
		case <-time.After(30 * time.Second):
			log.Fatal("failed to connect after 30 seconds")
		case <-connectDone:

		}
	}()

	s := &server{
		modem:      m,
		inboundMsg: make(chan gogsm.Msg, 100),
	}
	s.initCMDS()

	err = m.Connect()
	if err != nil {
		log.Fatal(err)
	}

	close(connectDone)

	log.Printf("Read Messages")
	msgs, err := m.ReadMessages()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("read: %d msgs", len(msgs))
	for _, msg := range msgs {
		if !msg.Inbound {
			continue
		}
		fmt.Printf("msg: %+v\n", msg)
		err := s.sendMsg(msg)
		if err != nil {
			log.Fatalf("send msg err: %s", err)
		}

		m.DeleteMsg(msg.Index)
	}

	if *slackToken != "" && *slackChannelID != "" {
		log.Printf("starting slack worker")
		go s.runSlack()
	} else {
		log.Printf("slack integration disabled")
	}

	log.Printf("Subscribe Msgs")
	ch, err := m.Subscribe(context.Background(), gogsm.EvtSMS)
	if err != nil {
		log.Fatalf("Subscribe err: %s", err)
	}

	for evt := range ch {
		fmt.Printf("Got %+v\n", evt)
		err = s.sendMsg(evt.Msg)
		if err != nil {
			log.Fatalf("send msg err: %s", err)
		}

		m.DeleteMsg(evt.Msg.Index)
	}
}

type server struct {
	cmds       []cmd
	modem      *gogsm.Modem
	inboundMsg chan gogsm.Msg
}

func (s *server) sendMsg(msg gogsm.Msg) error {
	pm := PostMsg{
		From: msg.From,
		To:   msg.To,
		TS:   msg.TS,
		Body: msg.Body,
	}

	select {
	case s.inboundMsg <- msg:
	default:
	}

	payload, err := json.Marshal(pm)
	if err != nil {
		return fmt.Errorf("Marshal msg err: %s", err)
	}

	req, err := http.NewRequest("POST", *postURL, bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("NewRequest err: %s", err)
	}

	req.SetBasicAuth(*username, *password)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("Post error: %s", err)
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("Post error: %d %+v", resp.StatusCode, resp)
	}

	return nil

}

type PostMsg struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	TS   time.Time `json:"ts"`
	Body string    `json:"body"`
}
