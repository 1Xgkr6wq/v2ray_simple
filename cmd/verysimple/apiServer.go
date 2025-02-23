package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"flag"
	"net/http"
	"time"

	"github.com/e1732a364fed/v2ray_simple/tlsLayer"
	"github.com/e1732a364fed/v2ray_simple/utils"
)

var (
	enableApiServer     bool
	apiServerRunning    bool
	apiServerPathPrefix string
)

func init() {
	flag.BoolVar(&enableApiServer, "ea", false, "enable api server")
	flag.StringVar(&apiServerPathPrefix, "spp", "/api", "api Server Path Prefix, must start with '/' ")

}

//阻塞
func checkConfigAndTryRunApiServer() {
	if standardConf.App == nil {
		return
	}

	if ap := standardConf.App.AdminPass; ap != "" {
		runApiServer(ap)
	}
}

//阻塞
func runApiServer(adminUUID string) {

	utils.Info("Start Api Server")

	ser := newApiServer("admin", adminUUID)

	mux := http.NewServeMux()
	mux.HandleFunc(apiServerPathPrefix+"/allstate", ser.basicAuth(func(w http.ResponseWriter, r *http.Request) {
		printAllState(w)
	}))

	srv := &http.Server{
		Addr:         "127.0.0.1:48345",
		Handler:      mux,
		IdleTimeout:  time.Minute,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
			Certificates:       tlsLayer.GenerateRandomTLSCert(),
		},
	}

	apiServerRunning = true
	srv.ListenAndServeTLS("", "")
	apiServerRunning = false
}

type auth struct {
	expectedUsernameHash [32]byte
	expectedPasswordHash [32]byte
}

type apiServer struct {
	admin_auth auth
}

func newApiServer(user, pass string) *apiServer {
	s := new(apiServer)

	s.admin_auth.expectedUsernameHash = sha256.Sum256([]byte(user))
	s.admin_auth.expectedPasswordHash = sha256.Sum256([]byte(pass))
	return s
}

func (ser *apiServer) basicAuth(realfunc http.HandlerFunc) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		thisun, thispass, ok := r.BasicAuth()
		if ok {
			usernameHash := sha256.Sum256([]byte(thisun))
			passwordHash := sha256.Sum256([]byte(thispass))

			usernameMatch := (subtle.ConstantTimeCompare(usernameHash[:], ser.admin_auth.expectedUsernameHash[:]) == 1)
			passwordMatch := (subtle.ConstantTimeCompare(passwordHash[:], ser.admin_auth.expectedPasswordHash[:]) == 1)

			if usernameMatch && passwordMatch {
				realfunc.ServeHTTP(w, r)
				return
			}
		}

		w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}
