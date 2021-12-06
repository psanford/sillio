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

func main() {

	flag.Parse()
	m, err := gogsm.NewModem(*modem)
	if err != nil {
		log.Fatal(err)
	}
	err = m.Connect()
	if err != nil {
		log.Fatal(err)
	}

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
		err := sendMsg(msg)
		if err != nil {
			log.Fatalf("send msg err: %s", err)
		}

		m.DeleteMsg(msg.Index)
	}

	log.Printf("Subscribe Msgs")
	ch, err := m.Subscribe(context.Background(), gogsm.EvtSMS)
	if err != nil {
		log.Fatalf("Subscribe err: %s", err)
	}

	for evt := range ch {
		fmt.Printf("Got %+v\n", evt)
		err = sendMsg(evt.Msg)
		if err != nil {
			log.Fatalf("send msg err: %s", err)
		}

		m.DeleteMsg(evt.Msg.Index)
	}
}

func sendMsg(msg gogsm.Msg) error {
	pm := PostMsg{
		From: msg.From,
		To:   msg.To,
		TS:   msg.TS,
		Body: msg.Body,
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
