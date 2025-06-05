# Self-hosted GitHub Runner

This is a lightweight service that's used as part of our Synthetics CI/CD for the `sematext-cloud` repo. It's designed to run persistently as a server on one of our kubernetes clusters and listen for requests. You can use it as an example on how to convert GitHub workflows to self-hosted applications which you can run on your existing environments and circumvent GitHub Actions Minutes limitations, since it also showcases how to interact with GitHub from external services.

These requests are sent from ArgoCD when an environment deployment is complete. This service then collects the latest commit hash for the deployed environment from its config on the `deployment` repo so that it can be linked with the appropriate commit on the `sematext-cloud` repo. This information is then sent to the `sematext-cloud` repo as a `repository_dispatch` event.



## Running locally

There are two ways to run the self-hosted runner locally for testing. Running the Go program is easier when making changes to the code, but once those are done you should also run the Dockerized setup to confirm everything works that way, since we'll be using it as a Docker container in production.


### Go program

```bash
go build main.go
go run main.go
```


### Dockerized setup

The base image we're using is very light, so the building process shouldn't take longer than a couple of seconds.

```bash
docker build -t gh-runner .
docker run --name gh-runner -p 9555:9555 gh-runner
```



## Authorization for private repositories

A GitHub token with appropriate permissions to access both the `deployment` repo and the `sematext-cloud` repo is required. It will be automatically picked up by the service if it's set as an environment variable called `GITHUB_TOKEN`, but it can also be passed within a request to the `/dispatch` endpoint for testing purposes.

**Note:** We can use the PAT for `build@sematext.com` here, since it has access to both of the repos.



## Environment variables

| Variable name    | Default value              |
|------------------|----------------------------|
| PORT             | `9555`                     |
| GITHUB_API_URL   | `https://api.github.com`   |
| TARGET_REPO      | `sematext/sematext-cloud`  |
| DEPLOYMENT_REPO  | `sematext/deployment`      |
| GITHUB_TOKEN     | (empty)                    |



## API specification

### POST `/dispatch`

Processes deployment notifications and triggers repository dispatch events.

**Request:**
```json
{
  "application": "pr-1234",
  "github_token": "ghp_xxxxxxxxxxxx"
}
```

**Request Fields:**

| Field Name | Type | Required | Description |
|------------|------|----------|-------------|
| `application` | string | Yes | Application name. Must start with `pr-` to be processed. |
| `github_token` | string | No | GitHub token for authentication. If not provided, the service will use the `GITHUB_TOKEN` environment variable. |

#### Response Cases

**Success - 200**
```json
{
  "status": "success",
  "commitHash": "abc123def456",
  "sourceName": "pr-example-app"
}
```

**Skipped (Invalid Application Name) - 200**
```json
{
  "status": "skipped",
  "reason": "application name doesn't start with 'pr-'"
}
```

#### Error Responses
- `400 Bad Request` - Invalid JSON payload or missing GitHub token
- `404 Not Found` - Could not find `values.yaml` for the specified application
- `405 Method Not Allowed` - Non-POST request
- `500 Internal Server Error` - Error extracting deployment tag or sending dispatch

#### Behavior
1. Validates that the application name starts with `pr-`
2. Searches for `values.yaml` in `deployment` repository paths:
   - `configs/pr/light/{application}/values.yaml`
   - `configs/pr/heavy/{application}/values.yaml`
3. Extracts the `DEPLOYMENT_TAG` from the `values.yaml` file
4. Sends a `repository_dispatch` event with type `environment_ready` to the target repository (`sematext-cloud`)


### GET `/health`

Health check endpoint for monitoring service availability.

#### Response Codes

**Success - 200**
```json
{
  "status": "healthy"
}
```
