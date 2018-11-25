package main

import (
	"encoding/json"
	"fmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/xanzy/go-gitlab"
	"go.etcd.io/bbolt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"time"
)

const defaultBoltDataPath = "/tmp/gitlab-mr-coverage.db"
const defaultPort = "4040"
const gitlabBaseURL = "https://gitlab.monterosa.co.uk"
const gitlabToken = "sometoken"

var db *bolt.DB
var git *gitlab.Client

func main() {
	zerolog.TimeFieldFormat = ""
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	log.Info().Msg("Gitlab Merge Request Coverage reporter")

	db = prepareDatabase(defaultBoltDataPath)
	git = prepareGitlabClient(gitlabBaseURL, gitlabToken)

	port := os.Getenv("PORT")
	if len(port) == 0 {
		port = defaultPort
	}

	startWebhookListener(port)
}

func startWebhookListener(port string) {
	http.HandleFunc("/", webhookHandler)

	log.Info().Msgf("Starting webhook listener on port %s", port)

	err := http.ListenAndServe(fmt.Sprintf(":%s", port), nil)
	if err != nil {
		log.Fatal().Err(err).Msgf("Failed to start webhook listener")
	}

}

func prepareDatabase(boltDataPath string) *bolt.DB {
	db, err := bolt.Open(boltDataPath, 0600, &bolt.Options{Timeout: 1 * time.Second})

	if err != nil {
		log.Fatal().Err(err).Msgf("Failed to open BoltDB file %s", boltDataPath)
	}

	log.Info().Msgf("BoltDB data file path: %s", db.Path())

	return db
}

func prepareGitlabClient(gitlabBaseURL, gitlabToken string) *gitlab.Client {
	git := gitlab.NewClient(nil, gitlabToken)

	err := git.SetBaseURL(gitlabBaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("Setting base gitlab URL failed")
	}

	return git
}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	log.Debug().Msg("Received webhook request")
	payload, err := ioutil.ReadAll(r.Body)

	if err != nil {
		log.Error().Err(err).Msg("Error while reading request body")
		return
	}

	event, err := gitlab.ParseWebhook(gitlab.WebhookEventType(r), payload)

	if err != nil {
		log.Error().Err(err).Msg("Webhook cannot be parsed")
		return
	}

	db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("events"))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}

		id, _ := bucket.NextSequence()
		buf, _ := json.Marshal(event)

		bucket.Put([]byte(strconv.FormatUint(id, 10)), buf)

		return nil
	})

	switch event := event.(type) {
	case *gitlab.MergeEvent:
		handleMergeRequestEvent(event)
	default:
		log.Debug().Msg("Skipping")
	}
}

func handleMergeRequestEvent(event *gitlab.MergeEvent) {
	log := log.With().
		Int("merge_request", event.ObjectAttributes.IID).
		Logger()

	log.Info().
		Interface("event", event).
		Msg("Merge request event received")

	projectID := event.ObjectAttributes.TargetProjectID
	lastCommitSHA := event.ObjectAttributes.LastCommit.ID

	git := gitlab.NewClient(nil, "sometoken")

	err := git.SetBaseURL("https://gitlab.monterosa.co.uk")
	if err != nil {
		log.Error().Err(err).Msg("Setting base gitlab URL failed")
		return
	}

	noteOpts := &gitlab.CreateMergeRequestNoteOptions{
		Body: gitlab.String(fmt.Sprintf("Detected last commit: %s", lastCommitSHA)),
	}

	note, _, err := git.Notes.CreateMergeRequestNote(projectID, event.ObjectAttributes.IID, noteOpts)

	if err != nil {
		log.Error().Err(err).Msg("Cannot create note on merge request")
	}

	log.Info().
		Interface("note", note).
		Msg("Note created")
}
