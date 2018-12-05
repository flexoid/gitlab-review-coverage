# GitLab Merge Request coverage reporter

The purpose of this tool is to report test coverage changes introduced by the merge request.

Coverage can be increased, decreased or remained the same.

## Requirements

### GitLab version

Checked and running on self-hosted GitLab CE 9.2.0.
Should work on the more recent versions.

### Running utility

The following ENV variables must be set:

* `GITLAB_BASE_URL` - Base URL for API calls (e.g. `https://gitlab.com`)
* `GITLAB_TOKEN` - GitLab API token (e.g. `SR-B7fFehUD91g7ygua`)
* `PORT` - available port on the host system to listen for incoming GitLab webhook
* `BOLT_DB_PATH` - path to the BoltDB file

### GitLab project configuration

GitLab webhook must be configured on the project.

Webhook URL is the URL to the running instance of this utility, with specified `PORT`.
Enabled triggers: "Merge Requests Events", "Build Events".

Test CI jobs must have configured coverage RegExp.

## Workflow overview

* Build event is received.
  * The commit coverage is stored, if available for that job.

  * If there are one or more merge requests available with this commit as the latest MR commit,
    the note is opened or updated with the coverage change info.

* Merge Request event is received.
  * Fetch and store merge request related data:
    * last MR commit
    * last commit before MR
  * If there's coverage data already stored for both that commits,
    coverage change can be easily determined and posted via opened/updated note.

The ID of the new note is stored for the merge request,
so it can be used to update it later when the latest MR commit is changed,
instead of posting a new one.

## Technical details

The utility uses BoltDB to store all required data to operate.

All the output of the utility is a structured log in JSON format,
which enables great debugging capabilities.

When the webhook event is received, as recommended, all the further processing is performed
in the separate goroutines to minimize webhook response time.
