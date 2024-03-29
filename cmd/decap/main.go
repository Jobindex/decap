package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jobindex/decap"
)

const (
	browsePath    = "/api/browse/"
	newBrowsePath = "/api/decap/v0/browse"
	DefaultPort   = 4531
	minAPI        = "v0.8"
	nextAPI       = "v0.9"
)

var (
	deprecatedAPIs []string
	debugMode      = false
)

func init() {
	deprecatedAPIs = inferDeprecatedAPIs()
	debugMode = os.Getenv("DEBUG") == "true"
}

func main() {
	flag.Parse()

	go decap.AllocateSessions()

	var handler http.Handler
	http.HandleFunc("/", http.NotFound)

	handler = handleHTTPMethod(http.HandlerFunc(oldVersionFmtBrowseHandler))
	http.Handle(browsePath, handler)

	handler = handleHTTPMethod(http.HandlerFunc(browseHandler))
	http.Handle(newBrowsePath, handler)

	handler = handleHTTPMethod(http.HandlerFunc(deprecationHandler))
	for _, v := range deprecatedAPIs {
		http.Handle(fmt.Sprintf("%s%s/", browsePath, v), handler)
	}

	var port int
	if debugMode {
		port = autoDebuggingPort()
	} else {
		port = DefaultPort
	}

	fmt.Fprintf(os.Stderr, "%s decap listening on http://localhost:%d%s\n",
		time.Now().Format("[15:04:05]"), port, newBrowsePath)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}

func oldVersionFmtBrowseHandler(w http.ResponseWriter, req *http.Request) {

	// validate version

	version, err := versionFromPath(req.URL.Path)
	if err != nil {
		status := http.StatusNotFound
		msg := fmt.Sprintf("%s: %s", http.StatusText(status), err)
		http.Error(w, msg, status)
		return
	}
	if version != minAPI && version != nextAPI {
		status := http.StatusNotFound
		msg := fmt.Sprintf("%s: non-existent API version: \"%s\"", http.StatusText(status), version)
		http.Error(w, msg, status)
		return
	}

	browseHandler(w, req)
}

func browseHandler(w http.ResponseWriter, req *http.Request) {

	// validate request

	if req.Header.Get("Content-Type") != "application/json" {
		status := http.StatusBadRequest
		msg := fmt.Sprintf("%s: expected application/json", http.StatusText(status))
		http.Error(w, msg, status)
		return
	}

	var dec decap.Request
	err := dec.ParseRequest(req.Body)
	if err != nil {
		status := http.StatusBadRequest
		msg := fmt.Sprintf("%s: %s", http.StatusText(status), err)
		http.Error(w, msg, status)
		return
	}

	// execute query

	err_status := http.StatusInternalServerError
	var res *decap.Result
	res, err = dec.Execute()
	if err != nil {
		// TODO: Propagate HTTP status properly
		msg := fmt.Sprintf("%s: %s", http.StatusText(err_status), err)
		http.Error(w, msg, err_status)
		return
	}

	// send response body
	switch res.Type() {
	case "json":
		w.Header().Set("Content-Type", "application/json")
		err = json.NewEncoder(w).Encode(res)
		if err != nil {
			msg := fmt.Sprintf("%s: %s", http.StatusText(err_status), "Couldn't encode response")
			http.Error(w, msg, err_status)
		}
		return
	case "pdf":
		w.Header().Set("Content-Type", "application/pdf")
		_, err = w.Write(res.PDFBuffer())
		if err != nil {
			msg := fmt.Sprintf("%s: %s",
				http.StatusText(err_status), "Couldn't write response bytes")
			http.Error(w, msg, err_status)
		}
		return
	case "png":
		w.Header().Set("Content-Type", "image/png")
		_, err = w.Write(res.ImgBuffer())
		if err != nil {
			msg := fmt.Sprintf("%s: %s",
				http.StatusText(err_status), "Couldn't write response bytes")
			http.Error(w, msg, err_status)
		}
		return
	default:
		fmt.Fprintf(os.Stderr, `unknown results type "%s"`, res.Type())
		msg := fmt.Sprintf(`%s: Unknown result type "%s"`,
			http.StatusText(err_status), res.Type())
		http.Error(w, msg, err_status)
		return
	}
}

func deprecationHandler(w http.ResponseWriter, req *http.Request) {
	version, _ := versionFromPath(req.URL.Path)
	status := http.StatusGone
	msg := fmt.Sprintf("%s: deprecated API version: %s", http.StatusText(status), version)
	http.Error(w, msg, status)
}

func versionFromPath(path string) (string, error) {
	segments := strings.Split(strings.TrimPrefix(path, browsePath), "/")
	if len(segments) == 0 || segments[0] == "" {
		return "", fmt.Errorf("want path format \"%s<version>/...\"", browsePath)
	}
	return segments[0], nil
}

func handleHTTPMethod(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {

		// TODO: Handle OPTIONS

		// TODO: Handle 301 redirects correctly (e.g. "/api/browse//")

		if req.Method != http.MethodPost {
			status := http.StatusMethodNotAllowed
			msg := fmt.Sprintf("%s: %s", http.StatusText(status), req.Method)
			http.Error(w, msg, status)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func inferDeprecatedAPIs() []string {
	var deprecated []string
	var minAPIMajor, minAPIMinor uint
	_, err := fmt.Sscanf(minAPI, "v%d.%d", &minAPIMajor, &minAPIMinor)
	if err != nil {
		log.Fatalf("malformed minimum API: %s", err)
	}
	for major := uint(0); major < minAPIMajor; major++ {
		for minor := 0; minor < 10; minor++ {
			deprecated = append(deprecated, fmt.Sprintf("v%d.%d", major, minor))
		}
	}
	for minor := uint(0); minor < minAPIMinor; minor++ {
		deprecated = append(deprecated, fmt.Sprintf("v%d.%d", minAPIMajor, minor))
	}
	return deprecated
}

func autoDebuggingPort() int {
	return DefaultPort - DefaultPort%1000 + 100 + os.Getuid()%100
}
