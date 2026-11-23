---
title: Media storage
parent: Administration
nav_order: 9
permalink: /administration/media
---

# Media storage

Uploaded media and copies of the media of other servers are kept under `media_api.base_path`. Files are stored by the hash of their content, so the same file uploaded twice, or held both as a local and as a remote media, is on the disk once, and is deleted with the last media that uses it.

## Quota

`media_api.max_user_storage_bytes` is how much media one local user may have stored at the same time, counted over all their uploads (each media ID counts, also when the file is shared). An upload that would go above it is refused with a 400 and `M_RESOURCE_LIMIT_EXCEEDED`. 0, the default, is no limit. The check is not atomic: uploads of one user that run at the same time can go a little above the limit. There is no exception for administrators.

The size of a single upload is limited by `media_api.max_file_size_bytes`.

## Retention

`media_api.retention` deletes media that nobody has downloaded for a while:

```yaml
media_api:
  retention:
    local_media_lifetime: 0s      # uploads of users of this server; 0 keeps them for ever
    remote_media_lifetime: 720h   # copies of media of other servers; 0 keeps them for ever
    interval: 1h                  # time between two runs
```

- A lifetime counts from the last download, or from the upload if there was none. The time of a download is written at most once an hour per media, so a lifetime of less than a few hours is not precise.
- Deleted **remote** media is fetched again from its server when somebody asks for it. Deleted **local** media is gone for good: only set `local_media_lifetime` if that is what you want, and say so to your users.
- The thumbnails of the media go with it.
- Quarantined media is never deleted by the retention, so that a copy of it cannot be fetched again. Delete it with the admin API.
- Media downloaded before this feature existed counts as unused since its upload, so the first run after turning it on can delete a lot.
- The run is logged at info level when it deleted something ("Removed unused media").

Retention of messages is a separate feature, see [message retention](13_message_retention.md).

## Admin API

Quarantine and deletion of single media or the media of a user are in [the Synapse admin API](8_synapse_adminapi.md).
