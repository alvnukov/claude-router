package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

func (u *uiServer) profileActivateAPI(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := u.cs.ActivateProfile(name, u.hl); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"active_profile": name}) // the client may be gone
}

func (u *uiServer) profileActivate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.renderSettingsResult(w, err, "")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	u.renderSettingsResult(w, u.cs.ActivateProfile(name, u.hl), "Профиль активирован: "+name)
}

func (u *uiServer) profileCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.renderSettingsResult(w, err, "")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	clone := r.FormValue("mode") == "clone"
	if r.FormValue("mode") != "clone" && r.FormValue("mode") != "empty" {
		http.Error(w, "unknown mode", http.StatusBadRequest)
		return
	}
	u.renderSettingsResult(w, u.cs.CreateProfile(name, clone), "Профиль создан: "+name)
}

func (u *uiServer) profileDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.renderSettingsResult(w, err, "")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	u.renderSettingsResult(w, u.cs.DeleteProfile(name), "Профиль удалён: "+name)
}
