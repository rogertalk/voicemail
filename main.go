package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/datastore"
	"google.golang.org/api/iterator"
)

const (
	ConfigPath       = "./config.json"
	TwilioFromNumber = "+14427776437"
	TwilioKeySid     = "_REMOVED_"
	TwilioKeySecret  = "_REMOVED_"
	TwilioMessages   = "https://api.twilio.com/2010-04-01/Accounts/_REMOVED_/Messages"
)

const Response = `<?xml version="1.0" encoding="UTF-8"?>
<Response>
	<Say>Please leave a message after the tone.</Say>
	<Record maxLength="30" />
	<Say>Sorry, no message could be recorded.</Say>
</Response>`

const VoicemailText = `You have new voicemail in Roger. First, please verify your phone number to listen.
Open Roger > Settings > Connect accounts > Add phone number.
http://rgr.im/get`

type Config struct {
	ListenAddr  string
	ProjectId   string
	AccessToken string
}

var (
	config    Config
	ctx       = context.Background()
	store     *datastore.Client
	apiURL, _ = url.Parse("https://api.rogertalk.com/v17/")
)

type Identity struct {
	Account   *datastore.Key `datastore:"account"`
	Available bool           `datastore:"available"`
	Status    string         `datastore:"status"`
}

type Participant struct {
	Id int64
}

type PendingVoicemail struct {
	From      string `datastore:"from",noindex`
	To        string `datastore:"to"`
	AudioURL  string `datastore:"audio_url",noindex`
	Delivered bool   `datastore:"delivered"`
}

type Stream struct {
	Id     int64
	Others []Participant
}

func main() {
	// Load configuration from file.
	data, err := ioutil.ReadFile(ConfigPath)
	if err != nil {
		log.Fatalf("Failed to load config (ioutil.ReadFile: %v)", err)
	}
	err = json.Unmarshal(data, &config)
	if err != nil {
		log.Fatalf("Failed to load config (json.Unmarshal: %v)", err)
	}

	// Set up the Datastore client.
	store, err = datastore.NewClient(ctx, config.ProjectId)
	if err != nil {
		log.Fatalf("Failed to create Datastore client (datastore.NewClient: %v)", err)
	}

	// Set up server for handling incoming requests.
	http.HandleFunc("/v1/call", callHandler)

	log.Printf("Starting server on %s...", config.ListenAddr)
	if err := http.ListenAndServe(config.ListenAddr, nil); err != nil {
		log.Fatalf("Failed to serve (http.ListenAndServe: %v)", err)
	}
}

func callHandler(w http.ResponseWriter, r *http.Request) {
	defer logRequestTime(r.Method, r.URL.Path, time.Now())
	// GET requests don't contain the recording.
	if r.Method == "GET" {
		log.Printf("Incoming call: %s", r.URL.Query())
		w.Write([]byte(Response))
		return
	}
	err := r.ParseForm()
	if err != nil {
		log.Printf("Failed to parse body: %v", err)
		return
	}
	from, to := r.Form.Get("From"), r.Form.Get("ForwardedFrom")
	audioURL := r.Form.Get("RecordingUrl")
	if ext := path.Ext(audioURL); ext == "" || ext == ".wav" {
		// Using MP3 directly is faster.
		audioURL = audioURL[:len(audioURL)-len(ext)] + ".mp3"
	}
	log.Printf("%s -> %s (%s)", from, to, audioURL)
	err = deliverVoicemail(from, to, audioURL, false)
	if err != nil {
		log.Printf("Failed to deliver voicemail: %v", err)
	}
}

func deliverPendingVoicemail(key *datastore.Key, voicemail PendingVoicemail) (err error) {
	err = deliverVoicemail(voicemail.From, voicemail.To, voicemail.AudioURL, true)
	if err != nil {
		return
	}
	voicemail.Delivered = true
	_, err = store.Put(ctx, key, &voicemail)
	return
}

func deliverVoicemail(from, to, audioURL string, retrying bool) (err error) {
	if to == "" {
		return fmt.Errorf("empty recipient (did someone call us?)")
	}
	if from == "" {
		from = "unknownuser"
	}
	fromIdentity, toIdentity, err := getIdentityPair(from, to)
	if toIdentity == nil || toIdentity.Available {
		if retrying {
			// The voicemail is already in the queue, so don't add it.
			return fmt.Errorf("retried delivery but %s still doesn't have an account", to)
		}
		pending := PendingVoicemail{
			From:     from,
			To:       to,
			AudioURL: audioURL,
		}
		key, storeErr := store.Put(ctx, datastore.IncompleteKey("PendingVoicemail", nil), &pending)
		if storeErr != nil {
			err = fmt.Errorf("receiver %s doesn't have an account, failed to store pending voicemail: %v", to, storeErr)
		} else {
			err = fmt.Errorf("receiver %s doesn't have an account, stored pending voicemail (%d)", to, key.ID)
		}
		return
	}
	toId := toIdentity.Account.ID
	var fromId int64
	if fromIdentity != nil && !fromIdentity.Available {
		fromId = fromIdentity.Account.ID
		_, err = postStream(fromId, 0, url.Values{
			"participant": {strconv.FormatInt(toId, 10)},
			"audio_url":   {audioURL},
		})
		return
	}
	// Sender has no account, so we need to create the stream in "reverse" first.
	// TODO: Give the sender a formatted display name from Twilio.
	stream, err := postStream(toId, 0, url.Values{
		"participant": {from},
		"reason":      {"voicemail"},
	})
	if err != nil {
		return
	}
	if len(stream.Others) > 0 {
		fromId = stream.Others[0].Id
	} else {
		// This is a monologue stream (the person left themselves a voicemail).
		fromId = toId
	}
	_, err = postStream(fromId, stream.Id, url.Values{
		"audio_url": {audioURL},
	})
	return
}

func flushPendingQueue() {
	q := datastore.NewQuery("PendingVoicemail").Filter("delivered =", false)
	t := store.Run(ctx, q)
	for {
		var voicemail PendingVoicemail
		key, err := t.Next(&voicemail)
		if err == iterator.Done {
			break
		} else if err != nil {
			log.Printf("Failed to get a pending voicemail: %v", err)
			continue
		}
		if err := deliverPendingVoicemail(key, voicemail); err != nil {
			log.Printf("Failed to deliver a pending voicemail: %v", err)
		} else {
			log.Printf("Delivered pending voicemail to %s", voicemail.To)
		}
	}
}

func getIdentityPair(a, b string) (aa, bb *Identity, err error) {
	// Ugly hack due to strange design of the Go datastore package.
	// TODO: Clean up.
	aa = new(Identity)
	if err := store.Get(ctx, datastore.NameKey("Identity", a, nil), aa); err != nil {
		aa = nil
	}
	bb = new(Identity)
	if err := store.Get(ctx, datastore.NameKey("Identity", b, nil), bb); err != nil {
		bb = nil
	}
	return
}

func logRequestTime(method, path string, start time.Time) {
	log.Printf("Handled %s %s in %s", method, path, time.Since(start))
}

func postStream(accountId, streamId int64, fields url.Values) (stream *Stream, err error) {
	var path string
	if streamId > 0 {
		path = fmt.Sprintf("streams/%d/chunks", streamId)
	} else {
		path = "streams"
	}
	ref, err := url.Parse(path)
	if err != nil {
		return
	}
	streamsURL := apiURL.ResolveReference(ref)
	query := url.Values{
		"on_behalf_of": {strconv.FormatInt(accountId, 10)},
	}
	streamsURL.RawQuery = query.Encode()
	req, err := http.NewRequest("POST", streamsURL.String(), strings.NewReader(fields.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", config.AccessToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s (on behalf of %d) returned %s", req.URL.Path, accountId, resp.Status)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return
	}
	stream = new(Stream)
	err = json.Unmarshal(body, stream)
	return
}

func sendSMS(to, message string) (err error) {
	fields := url.Values{
		"From": {TwilioFromNumber},
		"To":   {to},
		"Body": {message},
	}
	req, err := http.NewRequest("POST", TwilioMessages, strings.NewReader(fields.Encode()))
	if err != nil {
		return
	}
	req.SetBasicAuth(TwilioKeySid, TwilioKeySecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		return fmt.Errorf("%s returned %s", req.URL.Path, resp.Status)
	}
	return
}
