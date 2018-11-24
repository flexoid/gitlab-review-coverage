package main

import (
	"fmt"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/xanzy/go-gitlab"
	"io/ioutil"
	"net/http"
	"os"
)

func main() {
	zerolog.TimeFieldFormat = ""
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	log.Info().Msg("Gitlab Merge Request Coverage reporter")

	port := os.Getenv("PORT")
	if len(port) == 0 {
		port = "4040"
	}
	log.Debug().Msg("Received webhook request")

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
