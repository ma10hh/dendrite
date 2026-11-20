---
title: Metrics
parent: Administration
nav_order: 10
permalink: /administration/metrics
---

# Metrics

With `global.metrics.enabled: true` Dendrite serves Prometheus metrics at `/metrics`, protected with the basic authentication from `global.metrics.basic_auth` if a username and a password are set (without them, protect the path in your reverse proxy). The Go runtime and process metrics (`go_*`, `process_*`) are included.

## What to watch

| Metric | Type | Meaning |
|---|---|---|
| `dendrite_http_request_duration_seconds{handler,code}` | histogram | How long each client, federation, media and admin handler took. Long polls (`sync`) include the time they waited |
| `dendrite_up{version}` | counter | 1 while the server runs, labelled with its version |
| `dendrite_syncapi_active_sync_requests`, `..._waiting_sync_requests` | gauge | Requests of `/sync` being handled, and those waiting for something to return |
| `dendrite_syncapi_sliding_sync_active_requests`, `..._waiting_requests`, `..._connections` | gauge | The same for sliding sync, and the connections held in memory |
| `dendrite_syncapi_calculateHistoryVisibility_duration_millis` | histogram | |
| `dendrite_clientapi_sendevent_duration_millis` | histogram | From a client sending an event until the roomserver has it |
| `dendrite_clientapi_reg_users_total` | counter | Registered users |
| `dendrite_roomserver_processroomevent_duration_millis` | histogram | How long the roomserver takes per event |
| `dendrite_roomserver_input_backpressure` | gauge | Events queued for the input of a room. Rising for long means the room cannot keep up |
| `dendrite_roomserver_calculate_state_*` | histogram, summary | State resolution time and sizes |
| `dendrite_roomserver_partial_state_rooms` | gauge | Rooms joined with partial state that are not whole yet |
| `dendrite_roomserver_partial_state_resyncs_total{result}` | counter | Attempts to make them whole: `success` or `failure` |
| `dendrite_federationapi_recv_pdus`, `..._recv_edus` | counter | Incoming events from other servers |
| `dendrite_federationapi_destination_queues_total`, `..._running`, `..._backing_off` | gauge | Sending queues to other servers. Many backing off means other servers are unreachable |
| `dendrite_keyserver_device_list_update`, `..._worker_backpressure`, `..._worker_servers_retrying` | counter, gauge | Device list updates from other servers |
| `dendrite_mediaapi_download*`, `dendrite_mediaapi_thumbnail*` | counter, histogram | Media requests and sizes |
| `dendrite_mediaapi_retention_deleted_total{kind}` | counter | Media deleted by the retention, `local` or `remote` |
| `dendrite_mediaapi_uploads_over_quota_total` | counter | Uploads refused because the user is out of storage |
| `dendrite_delegated_auth_introspections_total{result}` | counter | Tokens checked with the provider (MAS): `cache_hit`, `active`, `inactive`, `error`. `error` means the provider could not be asked, and the users get errors with it |
| `go_sql_*{db_name}` | gauge, counter | The connection pool of each database: open, idle and in use connections, and the time requests waited for one. `db_name` is the host and database, or the SQLite file, never the credentials |
| `dendrite_caching_ristretto_ratio`, `..._cost` | gauge | Hit ratio and size of the caches |

Useful alerts to start with: the error rate of `dendrite_http_request_duration_seconds` by `code=~"5.."`, the 99th percentile of the same for `handler="send_message"` and `handler="sync"`, `dendrite_roomserver_input_backpressure` above zero for minutes, `go_sql_wait_duration_seconds_total` rising, and `dendrite_delegated_auth_introspections_total{result="error"}`.

## Not covered

- Tracing is still opentracing with Jaeger (`global.tracing`); moving to OpenTelemetry is open (decision 26).
- There are no metrics for the JetStream consumers (how far each is behind); use the NATS monitoring port for that.
- Handlers registered through `MakeHTTPAPI` (HTML pages) have their own counter by status code, named after the handler.
- Logs are logrus with fields (`room_id`, `user_id`, `req.id` and so on); set `logging` in the configuration for files, and `DENDRITE_TRACE_HTTP=1` to log requests and responses while debugging.
