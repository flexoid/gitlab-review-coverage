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
	case *gitlab.BuildEvent:
		handleBuildEvent(event)
	default:
		log.Debug().Msg("Skipping event type")
	}
}

func handleMergeRequestEvent(event *gitlab.MergeEvent) {
	projectID := event.ObjectAttributes.TargetProjectID

	log := log.With().
		Int("project_id", projectID).
		Int("merge_request", event.ObjectAttributes.IID).
		Logger()

	log.Info().
		Interface("event", event).
		Msg("Merge request event received")

	lastCommitSHA := event.ObjectAttributes.LastCommit.ID

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

func handleBuildEvent(event *gitlab.BuildEvent) {
	projectID := event.ProjectID
	jobID := event.BuildID
	sha := event.Sha

	log := log.With().
		Int("project_id", projectID).
		Int("job", jobID).
		Str("sha", sha).
		Logger()

	log.Debug().
		Interface("event", event).
		Msg("Build event received")

	if event.BuildStatus != "success" {
		log.Debug().
			Str("status", event.BuildStatus).
			Msg("Skipping as status is not success")

		return
	}

	err := storeJobData(projectID, jobID, sha)
	if err != nil {
		log.Error().Err(err).Msg("Failed to store job data")
	}

	log.Info().Msg("Job stored")
}

func storeJobData(projectID int, jobID int, sha string) error {
	return db.Update(func(tx *bolt.Tx) error {
		projectBucket, err := tx.CreateBucketIfNotExists([]byte(fmt.Sprintf("projects:%d", projectID)))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}

		jobBucket, err := projectBucket.CreateBucketIfNotExists([]byte(fmt.Sprintf("job:%s", sha)))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}

		seqID, _ := jobBucket.NextSequence()
		err = jobBucket.Put([]byte(strconv.FormatUint(seqID, 10)), []byte(strconv.Itoa(jobID)))
		if err != nil {
			return fmt.Errorf("storing job ID: %s", err)
		}

		return nil
	})
}
