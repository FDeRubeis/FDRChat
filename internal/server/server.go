package server

import (
	"net/http"

	"fdrchat/internal/auth"
	"fdrchat/internal/events"
	"fdrchat/internal/message"
	"fdrchat/internal/session"
	"fdrchat/internal/stream"
	"fdrchat/internal/user"
)

type Deps struct {
	Users    *user.Store
	Messages *message.Store
	Sessions *session.Manager
	Hub      *stream.Hub
}

func NewRouter(d Deps) http.Handler {
	authHandler := auth.NewHandler(d.Users, d.Sessions)
	userHandler := user.NewHandler(d.Users)
	messageHandler := message.NewHandler(d.Messages, d.Sessions, d.Hub)
	eventsHandler := events.NewHandler(d.Hub, d.Sessions)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/register", authHandler.Register)
	mux.HandleFunc("POST /auth/login", authHandler.Login)
	mux.Handle("POST /auth/logout", d.Sessions.Require(http.HandlerFunc(authHandler.Logout)))
	mux.Handle("GET /me", d.Sessions.Require(http.HandlerFunc(authHandler.Me)))

	mux.Handle("GET /users", d.Sessions.Require(http.HandlerFunc(userHandler.List)))
	mux.Handle("POST /messages", d.Sessions.Require(http.HandlerFunc(messageHandler.Send)))
	mux.Handle("POST /messages/{id}/read", d.Sessions.Require(http.HandlerFunc(messageHandler.MarkRead)))
	mux.Handle("GET /conversations/{userID}/messages", d.Sessions.Require(http.HandlerFunc(messageHandler.Conversation)))
	mux.Handle("GET /events", d.Sessions.Require(http.HandlerFunc(eventsHandler.Stream)))

	return mux
}
