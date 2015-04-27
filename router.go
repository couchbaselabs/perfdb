package main

import (
	"github.com/gorilla/mux"
)

func newRouter(contoller *Controller) *mux.Router {
	r := mux.NewRouter()
	r.StrictSlash(true)

	r.HandleFunc("/", contoller.listDatabases).Methods("GET")
	r.HandleFunc("/{db}", contoller.listSources).Methods("GET")
	r.HandleFunc("/{db}/{source}", contoller.listMetrics).Methods("GET")
	r.HandleFunc("/{db}/{source}", contoller.addSamples).Methods("POST")
	r.HandleFunc("/{db}/{source}/{metric}", contoller.getRawValues).Methods("GET")
	r.HandleFunc("/{db}/{source}/{metric}/summary", contoller.getSummary).Methods("GET")
	r.HandleFunc("/{db}/{source}/{metric}/heatmap", contoller.getHeatMap).Methods("GET")
	r.HandleFunc("/{db}/{source}/{metric}/histo", contoller.getHistogram).Methods("GET")

	r.HandleFunc("/{db}/{source}/{metric}/linechart", getLineChart).Methods("GET")

	return r
}
