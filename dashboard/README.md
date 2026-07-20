# Dashboard (optional)

A minimal Streamlit frontend for the backend's `GET /orders` and
`GET /healthz` endpoints. Not part of the core deliverable (see `plan.md`
D3) — added as an extra, simple viewer on top of the existing API.

## Run locally

```bash
pip install -r requirements.txt
streamlit run app.py
```
Opens at `http://localhost:8501`. By default it talks to the backend at
`http://localhost:8080`; override with the `API_URL` env var if your
backend is running elsewhere:
```bash
API_URL=http://localhost:8080 streamlit run app.py
```
(On Windows PowerShell: `$env:API_URL="http://localhost:8080"; streamlit run app.py`)

## Run with Docker

```bash
docker build -t vaurd-dashboard .
docker run -p 8501:8501 -e API_URL=http://host.docker.internal:8080 vaurd-dashboard
```
`host.docker.internal` lets the container reach the Go service running on
your host machine (not inside Docker). Not wired into the root
`docker-compose.yml` — the backend service itself isn't containerized
either (it's run via `go run main.go`), so this is a standalone image you
build/run only if you want the dashboard containerized too.

## What's on each page

- **Orders Table** — calls `GET /orders` with `status`/`sort`/`limit`/`offset`
  controls in the sidebar-adjacent row, renders the result as a table.
  A `Refresh` button re-fetches (Streamlit reruns the whole script on any
  widget interaction, so changing any control also refreshes the data).
  Shows a spinner while loading and a clear error message for bad requests
  (400), server errors, or if the API is unreachable at all.
- **Health Check** — calls `GET /healthz`, shows a green "HEALTHY" or red
  "DOWN"/"UNHEALTHY" indicator depending on the response.
