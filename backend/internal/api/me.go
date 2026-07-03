package api

import (
	"net/http"

	"github.com/songguo/songguo/internal/store"
)

// meView is the whoami payload the SPA reads at sign-in to pick its shell. For a
// consumer key it also carries the scope (models the key may play) and budget so
// the UI can filter and, later, show remaining budget. No operator config here.
type meView struct {
	Role   string   `json:"role"`
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Scope  []string `json:"scope"`
	Budget *float64 `json:"budget"` // nil = unlimited
	Spend  float64  `json:"spend"`
}

// handleMe reports the authenticated identity. The admin key resolves to the
// operator role (full dashboard); a consumer key resolves to the user role
// (scoped playground only). Auth already happened in sharedAuthMiddleware.
func (a *api) handleMe(w http.ResponseWriter, r *http.Request) {
	if roleFrom(r) == roleUser {
		u, _ := userFrom(r)
		spend, err := a.store.SpendByUser(u.ID, nil)
		if err != nil {
			a.writeDataErr(w, "user spend", err)
			return
		}
		scope := u.Scope
		if scope == nil {
			scope = []string{}
		}
		writeJSON(w, http.StatusOK, meView{
			Role:   string(roleUser),
			ID:     u.ID,
			Name:   u.Name,
			Scope:  scope,
			Budget: u.Budget,
			Spend:  spend,
		})
		return
	}
	writeJSON(w, http.StatusOK, meView{
		Role:  string(roleAdmin),
		ID:    store.AdminUserID,
		Name:  "admin",
		Scope: []string{},
	})
}
