Roger Voicemail Service
=======================

Records forwarded calls from Twilio into a Roger conversation between caller and recipient.


Endpoints
---------

### `GET /v1/call`

Picks up incoming calls and asks the caller to record a message.


### `POST /v1/call`

Handles a completed call with attached audio recording.


Pushing a version
-----------------

```bash
./deploy
```
