package main

import (
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

var db *bolt.DB
var git *gitlab.Client

func main() {
	zerolog.TimeFieldFormat = ""
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})

	log.Info().Msg("Gitlab Merge Request Coverage reporter")

	port := getRequiredEnvVar("PORT")
	gitlabBaseURL := getRequiredEnvVar("GITLAB_BASE_URL")
	gitlabToken := getRequiredEnvVar("GITLAB_TOKEN")
	boltDBPath := getRequiredEnvVar("BOLT_DB_PATH")

	log.Info().Msgf("Working with GitLab: %s", gitlabBaseURL)

	db = prepareDatabase(boltDBPath)
	git = prepareGitlabClient(gitlabBaseURL, gitlabToken)

	startWebhookListener(port)
}

func getRequiredEnvVar(varName string) string {
	envVar := os.Getenv(varName)
	if len(envVar) == 0 {
		log.Fatal().Msgf("Expected ENV variable set: %s", varName)
	}
	return envVar
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
	mergeRequestID := event.ObjectAttributes.IID
	lastCommitSHA := event.ObjectAttributes.LastCommit.ID

	log := log.With().
		Int("project_id", projectID).
		Int("merge_request_id", mergeRequestID).
		Str("sha", lastCommitSHA).
		Logger()

	log.Debug().
		Interface("event", event).
		Msg("Merge request event received")

	go processMergeRequest(projectID, mergeRequestID, &log)
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

	go handleCommitCoverage(projectID, event, &log)
}

func handleCommitCoverage(projectID int, event *gitlab.BuildEvent, log *zerolog.Logger) {
	job, _, err := git.Jobs.GetJob(projectID, event.BuildID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to fetch job")
		return
	}

	coverage := job.Coverage
	log.Debug().Float64("coverage", coverage).Msg("Received job coverage")

	if coverage == 0 {
		return
	}

	err = storeCommitCoverage(projectID, event.Sha, coverage)
	if err != nil {
		log.Error().Err(err).Msg("Failed to store commit coverage")
		return
	}

	log.Info().Float64("coverage", coverage).Msg("Coverage is stored")

	log.Debug().Msg("Updating linked merge requests")
	handleLinkedMergeRequests(projectID, event.Sha, log)
}

func handleLinkedMergeRequests(projectID int, sha string, log *zerolog.Logger) {
	var mergeRequestIDs []int

	err := readFromCommitBucket(projectID, sha, func(commitBucket *bolt.Bucket) error {
		linkedMergeRequestsBucket := commitBucket.Bucket([]byte("mrs"))
		if linkedMergeRequestsBucket == nil {
			return nil
		}

		return linkedMergeRequestsBucket.ForEach(func(mergeRequestIDStr, _ []byte) error {
			mergeRequestID, _ := strconv.Atoi(string(mergeRequestIDStr))
			mergeRequestIDs = append(mergeRequestIDs, mergeRequestID)
			return nil
		})
	})

	if err != nil {
		log.Error().Err(err).Msg("Failed to get linked merge requests for commit")
		return
	}

	log.Info().
		Ints("merge_request_ids", mergeRequestIDs).
		Msg("Got linked MRs")

	for _, mergeRequestID := range mergeRequestIDs {
		log := log.With().Int("merge_request_id", mergeRequestID).Logger()

		beforeSHA, lastCommitSHA, err := getMergeRequestCommitsData(projectID, mergeRequestID)
		if err != nil {
			log.Error().Err(err).Msg("Failed to get merge request commits data")
			continue
		}

		log.Debug().
			Str("before_sha", beforeSHA).
			Str("after_sha", lastCommitSHA).
			Msg("Got merge request commits data")

		if len(beforeSHA) == 0 || len(lastCommitSHA) == 0 {
			continue
		}

		handleDiscussionPosting(projectID, mergeRequestID, beforeSHA, lastCommitSHA, &log)
	}
}

func storeCommitCoverage(projectID int, sha string, coverage float64) error {
	return storeCommitData(projectID, sha, func(bucket *bolt.Bucket) error {
		coverageStr := strconv.FormatFloat(coverage, 'f', -1, 64)
		err := bucket.Put([]byte("coverage"), []byte(coverageStr))

		if err != nil {
			return fmt.Errorf("store commit coverage error: %s", err)
		}

		return nil
	})
}

func storeMergeRequestToCommitLink(projectID int, mergeRequestID int, lastCommitSHA string) error {
	return storeCommitData(projectID, lastCommitSHA, func(commitBucket *bolt.Bucket) error {
		mergeRequestIDsBucket, err := commitBucket.CreateBucketIfNotExists([]byte("mrs"))
		if err != nil {
			return fmt.Errorf("create merge request IDs bucket error: %s", err)
		}

		// Only keys matter here, so put zero byte as value
		err = mergeRequestIDsBucket.Put([]byte(strconv.Itoa(mergeRequestID)), []byte("\x00"))
		if err != nil {
			return fmt.Errorf("store merge request ID into commit error: %s", err)
		}

		return nil
	})
}

func storeCommitData(projectID int, sha string, storeFn func(*bolt.Bucket) error) error {
	return db.Update(func(tx *bolt.Tx) error {
		projectBucket, err := tx.CreateBucketIfNotExists([]byte(fmt.Sprintf("projects:%d", projectID)))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}

		commitBucket, err := projectBucket.CreateBucketIfNotExists([]byte(fmt.Sprintf("sha:%s", sha)))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}

		return storeFn(commitBucket)
	})
}

func processMergeRequest(projectID int, mergeRequestID int, log *zerolog.Logger) {
	commits, _, err := git.MergeRequests.GetMergeRequestCommits(projectID, mergeRequestID, nil)
	if err != nil {
		log.Error().Err(err).Msg("Failed to fetch merge request commits")
		return
	}

	firstCommit := commits[len(commits)-1]
	lastCommit := commits[0]
	commitBeforeMergeRequestSHA := firstCommit.ParentIDs[0]

	err = storeMergeRequestData(projectID, mergeRequestID, commitBeforeMergeRequestSHA, lastCommit.ID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to store merge request data")
		return
	}

	err = storeMergeRequestToCommitLink(projectID, mergeRequestID, lastCommit.ID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to store merge request to commit link")
		return
	}

	log.Info().Msg("Merge request stored")

	handleDiscussionPosting(projectID, mergeRequestID, commitBeforeMergeRequestSHA, lastCommit.ID, log)
}

func storeMergeRequestData(projectID int, mergeRequestID int, beforeCommitSHA, lastCommitSHA string) error {
	return storeInMergeRequestBucket(projectID, mergeRequestID, func(mergeRequestBucket *bolt.Bucket) error {
		err := mergeRequestBucket.Put([]byte("beforeSHA"), []byte(beforeCommitSHA))
		if err != nil {
			return fmt.Errorf("storing before commit SHA: %s", err)
		}

		err = mergeRequestBucket.Put([]byte("lastCommitSHA"), []byte(lastCommitSHA))
		if err != nil {
			return fmt.Errorf("storing last commit SHA: %s", err)
		}

		return nil
	})
}

// Returns 0 if coverage doesn't stored.
// Returns error only if coverage retrieving is failed.
func getCommitCoverage(projectID int, sha string) (float64, error) {
	var coverage float64

	err := readFromCommitBucket(projectID, sha, func(commitBucket *bolt.Bucket) error {
		coverageBytes := commitBucket.Get([]byte("coverage"))
		if coverageBytes == nil {
			return nil
		}

		var err error
		coverage, err = strconv.ParseFloat(string(coverageBytes), 64)
		if err != nil {
			return fmt.Errorf("error while parsing coverage %q from db: %s", coverageBytes, err)
		}

		return nil
	})

	if err != nil {
		return 0, err
	}

	return coverage, nil
}

func handleDiscussionPosting(projectID int, mergeRequestID int, beforeCommitSHA, lastCommitSHA string, log *zerolog.Logger) {
	coverageBefore, err := getCommitCoverage(projectID, beforeCommitSHA)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get coverage before merge request")
	}
	log.Debug().
		Str("before_sha", beforeCommitSHA).
		Float64("coverage", coverageBefore).
		Msg("Coverage before")

	coverageAfter, err := getCommitCoverage(projectID, lastCommitSHA)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get coverage after merge request")
	}
	log.Debug().
		Str("after_sha", lastCommitSHA).
		Float64("coverage", coverageAfter).
		Msg("Coverage after")

	postOrUpdateCoverageMessage(projectID, mergeRequestID, coverageBefore, coverageAfter, log)
}

func postOrUpdateCoverageMessage(projectID, mergeRequestID int, coverageBefore, coverageAfter float64, log *zerolog.Logger) {
	message := noteMessage(coverageBefore, coverageAfter)

	existingNoteID, err := getNoteID(projectID, mergeRequestID)
	if err != nil {
		log.Error().Err(err).Msg("Failed to get existing note ID")
	}

	if existingNoteID == 0 {
		log.Info().Msg("Posting new note")

		noteID, err := postCoverageMessage(projectID, mergeRequestID, message, log)
		if err != nil {
			log.Error().Err(err).Msg("Cannot create note on merge request")
		}

		err = storeNoteID(projectID, mergeRequestID, noteID)
		if err != nil {
			log.Error().Err(err).Msg("Failed to store new note ID")
		}
	} else {
		log.Info().Int("note_id", existingNoteID).Msg("Modifying existing note")

		err := updateCoverageMessage(projectID, mergeRequestID, existingNoteID, message, log)
		if err != nil {
			log.Error().Err(err).Msg("Cannot update note on merge request")
		}
	}
}

func postCoverageMessage(projectID, mergeRequestID int, message string, log *zerolog.Logger) (int, error) {
	noteOpts := &gitlab.CreateMergeRequestNoteOptions{
		Body: gitlab.String(message),
	}

	note, _, err := git.Notes.CreateMergeRequestNote(projectID, mergeRequestID, noteOpts)
	if err != nil {
		return 0, err
	}

	log.Info().
		Int("note_id", note.ID).
		Str("note_message", message).
		Msg("Note created")

	return note.ID, nil
}

func updateCoverageMessage(projectID, mergeRequestID, noteID int, message string, log *zerolog.Logger) error {
	noteOpts := &gitlab.UpdateMergeRequestNoteOptions{
		Body: gitlab.String(message),
	}

	note, _, err := git.Notes.UpdateMergeRequestNote(projectID, mergeRequestID, noteID, noteOpts)
	if err != nil {
		return err
	}

	log.Info().
		Int("note_id", note.ID).
		Str("note_message", message).
		Msg("Note updated")

	return nil
}

func noteMessage(coverageBefore, coverageAfter float64) string {
	var message string

	if coverageBefore == 0 || coverageAfter == 0 {
		message = "Waiting for coverage info..."
	} else {
		coverageBeforeStr := strconv.FormatFloat(coverageBefore, 'f', -1, 64)
		coverageAfterStr := strconv.FormatFloat(coverageAfter, 'f', -1, 64)

		if coverageAfter > coverageBefore {
			message = fmt.Sprintf("Coverage is increased from %s%% to %s%%! :thumbsup:",
				coverageBeforeStr, coverageAfterStr)
		} else if coverageAfter < coverageBefore {
			message = fmt.Sprintf("Coverage is decreased from %s%% to %s%% :thumbsdown:",
				coverageBeforeStr, coverageAfterStr)
		} else {
			message = fmt.Sprintf("Coverage is the same: %s%% :muscle:", coverageAfterStr)
		}
	}

	return fmt.Sprintf("**Coverage reporter**  \n%s", message)
}

func storeNoteID(projectID, mergeRequestID, noteID int) error {
	return storeInMergeRequestBucket(projectID, mergeRequestID, func(mergeRequestBucket *bolt.Bucket) error {
		err := mergeRequestBucket.Put([]byte("note_id"), []byte(strconv.Itoa(noteID)))
		if err != nil {
			return fmt.Errorf("storing note ID error: %s", err)
		}
		return nil
	})
}

func storeInMergeRequestBucket(projectID int, mergeRequestID int, storeFn func(*bolt.Bucket) error) error {
	return db.Update(func(tx *bolt.Tx) error {
		projectBucket, err := tx.CreateBucketIfNotExists([]byte(fmt.Sprintf("projects:%d", projectID)))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}

		mergeRequestBucket, err := projectBucket.
			CreateBucketIfNotExists([]byte(fmt.Sprintf("mr:%d", mergeRequestID)))
		if err != nil {
			return fmt.Errorf("create bucket: %s", err)
		}

		return storeFn(mergeRequestBucket)
	})
}

func getNoteID(projectID, mergeRequestID int) (int, error) {
	var noteID int

	err := readFromMergeRequestBucket(projectID, mergeRequestID, func(mergeRequestBucket *bolt.Bucket) error {
		noteIDBytes := mergeRequestBucket.Get([]byte("note_id"))
		if noteIDBytes == nil {
			return nil
		}

		var err error
		noteID, err = strconv.Atoi(string(noteIDBytes))
		if err != nil {
			return err
		}

		return nil
	})

	return noteID, err
}

func getMergeRequestCommitsData(projectID, mergeRequestID int) (beforeSHA, lastSHA string, err error) {
	err = readFromMergeRequestBucket(projectID, mergeRequestID, func(mergeRequestBucket *bolt.Bucket) error {
		sha := mergeRequestBucket.Get([]byte("beforeSHA"))
		if sha != nil {
			beforeSHA = string(sha)
		}

		sha = mergeRequestBucket.Get([]byte("lastCommitSHA"))
		if sha != nil {
			lastSHA = string(sha)
		}

		return nil
	})

	return
}

func readFromMergeRequestBucket(projectID int, mergeRequestID int, readFn func(*bolt.Bucket) error) error {
	return db.View(func(tx *bolt.Tx) error {
		projectBucket := tx.Bucket([]byte(fmt.Sprintf("projects:%d", projectID)))
		if projectBucket == nil {
			return nil
		}

		mergeRequestBucket := projectBucket.Bucket([]byte(fmt.Sprintf("mr:%d", mergeRequestID)))
		if mergeRequestBucket == nil {
			return nil
		}

		return readFn(mergeRequestBucket)
	})
}

func readFromCommitBucket(projectID int, sha string, readFn func(*bolt.Bucket) error) error {
	return db.View(func(tx *bolt.Tx) error {
		projectBucket := tx.Bucket([]byte(fmt.Sprintf("projects:%d", projectID)))
		if projectBucket == nil {
			return nil
		}

		commitBucket := projectBucket.Bucket([]byte(fmt.Sprintf("sha:%s", sha)))
		if commitBucket == nil {
			return nil
		}

		return readFn(commitBucket)
	})
}
