package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"time"

	g "github.com/eonpatapon/contrail-gremlin/gremlin"
	"github.com/eonpatapon/contrail-gremlin/utils"
	"github.com/eonpatapon/gremlin"
	cli "github.com/jawher/mow.cli"
	logging "github.com/op/go-logging"
	"github.com/satori/go.uuid"
)

var (
	graphName  string
	log        = logging.MustGetLogger(os.Args[0])
	quit       = make(chan bool, 1)
	closed     = make(chan bool, 1)
	allImplems = map[string]func(Request, *App) ([]byte, error){
		"READALL_port":    listPorts,
		"READALL_network": listNetworks,
	}
)

type RequestOperation string

const (
	ListRequest = RequestOperation("READALL")
)

// RequestContext the context of incoming requests
type RequestContext struct {
	Type      string           `json:"type"`
	Operation RequestOperation `json:"operation"`
	TenantID  uuid.UUID        `json:"tenant_id"`
	UserID    uuid.UUID        `json:"user_id"`
	RequestID string           `json:"request_id"`
	IsAdmin   bool             `json:"is_admin"`
}

// RequestData the data of incoming requests
type RequestData struct {
	ID      string         `json:"id"`
	Fields  []string       `json:"fields"`
	Filters RequestFilters `json:"filters"`
}

type RequestFilters map[string][]interface{}

func (f RequestFilters) UnmarshalJSON(data []byte) (err error) {
	filters := make(map[string]interface{}, 0)
	if err := json.Unmarshal(data, &filters); err != nil {
		return nil
	}
	for k, v := range filters {
		switch v.(type) {
		case map[string]interface{}:
			for fk, fv := range v.(map[string]interface{}) {
				f[fk] = fv.([]interface{})
			}
		case []interface{}:
			f[k] = v.([]interface{})
		default:
			log.Errorf("Can't handle filter: %s=%+v (%T)", k, v, v)
		}
	}
	return nil
}

// Request the incoming request from neutron plugin
type Request struct {
	Context RequestContext
	Data    RequestData
}

// App the context shared by concurrent requests
type App struct {
	backend        *g.ServerBackend
	contrailClient *http.Client
	contrailAPIURL string
	quit           chan bool
	closed         chan bool
	methods        map[string]func(Request, *App) ([]byte, error)
}

func newApp(gremlinURI string, contrailAPISrv string, implems []string) *App {
	a := &App{
		contrailAPIURL: fmt.Sprintf("http://%s", contrailAPISrv),
		contrailClient: &http.Client{
			Timeout: 15 * time.Second,
		},
		backend: g.NewServerBackend(gremlinURI),
	}
	a.methods = make(map[string]func(Request, *App) ([]byte, error), 0)
	for _, implem := range implems {
		if method, ok := allImplems[implem]; ok {
			a.methods[implem] = method
			log.Noticef("Enabling implementation %s", implem)
		} else {
			log.Warningf("Implementation for %s not available", implem)
		}
	}
	a.backend.AddConnectedHandler(a.onGremlinConnect)
	a.backend.AddDisconnectedHandler(a.onGremlinDisconnect)
	a.backend.StartAsync()
	return a
}

func (a *App) onGremlinConnect() {
	log.Notice("Connected to gremlin-server")
}

func (a *App) onGremlinDisconnect(err error) {
	if err != nil {
		log.Warningf("Disconnected from gremlin-server: %s", err)
	} else {
		log.Notice("Disconnected from gremlin-server")
	}
}

func copyHeaders(src, dst http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func (a *App) forward(w http.ResponseWriter, r *http.Request, body io.Reader) {
	url := a.contrailAPIURL + r.URL.Path
	log.Debugf("Forwarding to %s", url)
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		log.Error(err.Error())
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	copyHeaders(r.Header, req.Header)
	resp, err := a.contrailClient.Do(req)
	if err != nil {
		log.Error(err.Error())
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()
	copyHeaders(resp.Header, w.Header())
	log.Debugf("Code: %d", resp.StatusCode)
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if err != nil {
		log.Errorf("Failed to copy response data")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (a *App) execute(query *gremlinQuery, bindings gremlin.Bind) ([]byte, error) {
	queryString := query.String()
	uuid, _ := uuid.NewV4()
	requestArgs := &gremlin.RequestArgs{
		Gremlin:  queryString,
		Language: "gremlin-groovy",
		Bindings: bindings,
	}
	if graphName != "g" {
		requestArgs.Aliases = map[string]string{
			"g": graphName,
		}
	}
	request := &gremlin.Request{
		RequestId: uuid.String(),
		Op:        "eval",
		Args:      requestArgs,
	}
	log.Debugf("Request: %+v", *requestArgs)
	res, err := a.backend.Send(request)
	if err != nil {
		return []byte{}, err
	}
	// TODO: check why gremlinClient does not return an empty list
	if len(res) == 0 {
		return []byte("[]"), nil
	}
	return res, nil
}

func (a *App) handler(w http.ResponseWriter, r *http.Request) {
	if !a.backend.IsConnected() {
		a.forward(w, r, r.Body)
		return
	}

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Errorf("Failed to read request: %s", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req := Request{
		Data: RequestData{
			Filters: make(RequestFilters, 0),
		},
	}
	if err := json.Unmarshal(body, &req); err != nil {
		log.Errorf("Failed to parse request %s: %s", string(body), err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	log.Debugf("Request: %+v\n", req)

	// Check if we have an implementation for this request
	handler, ok := a.methods[fmt.Sprintf("%s_%s", req.Context.Operation, req.Context.Type)]
	if ok {
		res, err := handler(req, a)
		if err != nil {
			log.Errorf("Handler hit an error: %s", err)
			w.WriteHeader(500)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte(fmt.Sprintf("%s", err)))
			return
		}
		log.Debugf("Response: %s", string(res))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(res)
	} else {
		a.forward(w, r, bytes.NewReader(body))
	}
}

func (a *App) stop() {
	a.backend.Stop()
}

func main() {
	app := cli.App(os.Args[0], "")
	gremlinSrv := app.String(cli.StringOpt{
		Name:   "gremlin",
		Value:  "localhost:8182",
		Desc:   "host:port of gremlin server",
		EnvVar: "GREMLIN_NEUTRON_GREMLIN_SERVER",
	})
	gremlinGraphName := app.String(cli.StringOpt{
		Name:   "gremlin-graph-name",
		Value:  "g",
		Desc:   "name of the graph traversal to use on the server",
		EnvVar: "GREMLIN_NEUTRON_GREMLIN_GRAPH_NAME",
	})
	contrailAPISrv := app.String(cli.StringOpt{
		Name:   "contrail-api",
		Value:  "localhost:8082",
		Desc:   "host:port of contrail-api server",
		EnvVar: "GREMLIN_NEUTRON_CONTRAIL_API_SERVER",
	})
	implems := app.Strings(cli.StringsOpt{
		Name:   "i implem",
		Value:  implemNames(),
		Desc:   "implementation to use",
		EnvVar: "GREMLIN_NEUTRON_IMPLEMENTATIONS",
	})
	utils.SetupLogging(app, log)
	app.Action = func() {
		gremlinURI := fmt.Sprintf("ws://%s/gremlin", *gremlinSrv)
		run(gremlinURI, *contrailAPISrv, *gremlinGraphName, *implems)
	}
	app.Run(os.Args)
}

func stop() {
	quit <- true
	<-closed
}

func run(gremlinURI string, contrailAPISrv string, gremlinGraphName string, implems []string) {
	graphName = gremlinGraphName

	app := newApp(gremlinURI, contrailAPISrv, implems)

	mux := http.NewServeMux()
	mux.HandleFunc("/neutron/", app.handler)

	srv := http.Server{
		Addr:         ":8080",
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 25 * time.Second,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt)

		select {
		case <-sigint:
		case <-quit:
		}

		// We received an interrupt signal, shut down.
		if err := srv.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			log.Errorf("HTTP server shutdown error: %s", err)
		} else {
			log.Notice("Stopped HTTP server")
		}
		close(idleConnsClosed)
	}()

	log.Notice("Starting HTTP server...")
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Errorf("HTTP server error: %s", err)
	}

	<-idleConnsClosed
	app.stop()
	closed <- true
}
