# FDRChat

FDRChat is a Go HTTP API for one-to-one chat, written as a take-home exercise. It allows users to register and exchange direct messages. 

User registration, authentication and session management are implemented following OWASP guidelines [1][2][3]. Session management uses a `__Host-session` cookie.  

The users and the messages with their status (saved, delivered, read) are stored in a PostgreSQL database. Users can start an SSE (Server-Sent Events) stream to receive notifications whenever they receive a new message. 

## Usage

Start the application and the database:
```sh
docker compose up
```
The API listens on <http://localhost:8080>. 

### Send and receive messages

```sh
# register two users
curl -s -X POST localhost:8080/auth/register -d '{"name":"alice","password":"s3cret-pass"}'
curl -s -X POST localhost:8080/auth/register -d '{"name":"bob","password":"s3cret-pass"}'

# log in and keep each session cookie in its own jar
curl -s -c alice.jar -X POST localhost:8080/auth/login -d '{"name":"alice","password":"s3cret-pass"}'
curl -s -c bob.jar   -X POST localhost:8080/auth/login -d '{"name":"bob","password":"s3cret-pass"}'

# display all user ids
curl -s -b alice.jar localhost:8080/users

# send a message from alice to bob 
curl -s -b alice.jar -X POST localhost:8080/messages \
  -d '{"to":"<bob-id>","body":"hello"}'

# read the conversation with alice as bob
curl -s -b bob.jar "localhost:8080/conversations/<alice-id>/messages?limit=50"

# use SSE to receive notifications
curl -sN -b bob.jar localhost:8080/events
```

Every route is registered in [`internal/server/server.go`](internal/server/server.go).

## API

| Method | Path | Session | What it does | Status |
| --- | --- | --- | --- | --- |
| `POST` | `/auth/register` | no | Create an account. | `201` |
| `POST` | `/auth/login` | no | Check the password, start a session, and set the cookie. | `200` |
| `POST` | `/auth/logout` | required | Delete this session and clear the cookie. | `204` |
| `GET` | `/me` | required | Return the signed-in user's profile. | `200` |
| `GET` | `/users` | required | List every registered user. | `200` |
| `POST` | `/messages` | required | Store a text message addressed to a user id. | `201` |
| `GET` | `/conversations/{userID}/messages` | required | Return the conversation with that user. | `200` |
| `POST` | `/messages/{id}/read` | required | Mark a received message as read. | `204` |
| `GET` | `/events` | required | Stream new messages and status changes for this session. | `200` |

## References

[1] https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html   
[2] https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html   
[3] https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html   