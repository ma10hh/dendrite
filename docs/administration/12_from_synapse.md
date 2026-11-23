---
title: Coming from Synapse
parent: Administration
nav_order: 12
permalink: /administration/from-synapse
---

# Coming from Synapse

There is **no tool that imports a Synapse database** into Dendrite. The two servers store data differently, and this fork does not try to read Synapse's. Moving means starting a new server, with these consequences:

- **Accounts**, devices, keys, rooms, history and media of Synapse do not move. Users sign in again on the new server and join their rooms again.
- **Staying on the same server name** is possible. Keep the signing key (see [signing keys](../installation/manual/3_signingkey.md): Synapse keys can be converted to the PEM format and used as `old_private_keys`, or as the main key), the user IDs `@alice:example.org` stay valid, and the other servers recognise the server. Rooms whose members are on other servers keep working over federation: when your users rejoin them, the history comes back from the other servers as far as they let new members see it (the history visibility of the room decides). Rooms where all members were on your server are gone, unless you keep Synapse running to read them.
- Users must create their accounts again with the **same localparts** to get the same user IDs. With the Matrix Authentication Service, MAS creates them (`/_synapse/mas/provision_user`) when they sign in.
- End-to-end encrypted history is lost: the room keys are on the devices of the users, and the server-side key backup and the cross-signing identity are in the Synapse database, which does not move. Only users who exported their room keys to a file before the move can import them again.
- **Media**: files can be copied by hand but the database records cannot; they would have to be uploaded again.

## What works the same

- The client API, for Element X and other clients, including sliding sync, and the federation API.
- The Synapse admin API, for the calls in [the Synapse admin API](8_synapse_adminapi.md): synapse-admin and Draupnir work with the users, rooms, media and registration tokens calls.
- The Matrix Authentication Service: use the `synapse` homeserver kind in MAS, see [delegated authentication](7_delegated_auth.md). `syn2mas`, the tool that imports a Synapse database into MAS, reads a Synapse database and cannot be used with Dendrite.

## What is different

- No workers: Dendrite is one process. Scale it up, not out.
- Message retention policies (`m.room.retention`) are supported, with fewer options, see [message retention](13_message_retention.md). Media is deleted by its own [retention](9_media.md), not by room.
- No SSO, SAML, CAS or JWT login by itself: use MAS for those.
- Shadow bans, rate limit overrides per user, account validity (set by an administrator, without renewal emails) and the rest of the Synapse admin API that is listed in [the Synapse admin API](8_synapse_adminapi.md) exist, with the differences described there; what is not listed does not.
- The configuration is a different file with different options: see `dendrite-sample.yaml`.
