package api

import (
	"errors"
	"net/http"

	"github.com/songguo/songguo/internal/parse"
	"github.com/songguo/songguo/internal/store"
)

// systemOneView is the typed question/answer structure of one System One call,
// read out of its captured bodies. It is the sibling of callMessagesData for a
// wire whose payload carries no system/tools/messages at all — the prompt view
// can only ever render three empty panels for it.
type systemOneView struct {
	Model     string                    `json:"model"`
	State     string                    `json:"state"`
	Questions []parse.SystemOneQuestion `json:"questions"`
	Answers   []parse.SystemOneAnswer   `json:"answers"`
	Tokens    parse.Tokens              `json:"tokens"`
}

func (a *api) handleCallSystemOne(w http.ResponseWriter, r *http.Request) {
	view, err := a.callSystemOneData(r.PathValue("id"))
	if err != nil {
		a.writeDataErr(w, "get call system one", err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// callSystemOneData parses one call's captured bodies as System One, or returns
// a *apiError (404) when no payload was captured for it.
//
// This is a point lookup on one row — the same read /messages and /trace
// already do from this page — not the session-wide read the shed rules bound,
// so it carries no a.shedding() gate and no bodyReads semaphore, matching
// handleCallMessages.
//
// A capture that is not a System One shape reads as an empty view rather than
// an error, exactly as callMessagesData does: the parse is best-effort by
// contract, and a body we could not read is not a missing call.
func (a *api) callSystemOneData(id string) (systemOneView, error) {
	p, err := a.store.GetPayload(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return emptySystemOneView(), notFoundErr("trace not found")
		}
		return emptySystemOneView(), err
	}

	reqBody := p.ReqBody
	if decoded, ok := decodeTraceBody(reqBody, headerValue(p.ReqHeaders, "Content-Encoding")); ok {
		reqBody = decoded
	}
	respBody := p.RespBody
	if decoded, ok := decodeTraceBody(respBody, headerValue(p.RespHeaders, "Content-Encoding")); ok {
		respBody = decoded
	}

	c, _ := parse.Parse(parse.Input{
		Wire:            "typesafe/systemone",
		ReqContentType:  p.ReqContentType,
		RespContentType: p.RespContentType,
		ReqBody:         reqBody,
		RespBody:        respBody,
	})
	view := emptySystemOneView()
	view.Model = c.Model
	view.Tokens = c.Tokens
	if c.SystemOne != nil {
		view.State = c.SystemOne.State
		if c.SystemOne.Questions != nil {
			view.Questions = c.SystemOne.Questions
		}
		if c.SystemOne.Answers != nil {
			view.Answers = c.SystemOne.Answers
		}
	}
	return view, nil
}

func emptySystemOneView() systemOneView {
	return systemOneView{
		Questions: []parse.SystemOneQuestion{},
		Answers:   []parse.SystemOneAnswer{},
	}
}
