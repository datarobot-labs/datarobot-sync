// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package syncer_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
)

// fakeAPI is an in-memory stand-in for the DataRobot Files API.
//
// It reproduces the behaviours dr-sync actually depends on, all of which were verified against
// the live service: fileChecksum is the SHA-256 of the stored bytes, allFiles pages at a
// bounded limit and must be walked via `next`, fromStage is atomic and rejects a name that
// already exists, downloads go through a URL that carries no credentials, and a prefix must end
// in a slash.
type fakeAPI struct {
	server *httptest.Server

	mu         sync.Mutex
	containers map[string]*fakeContainer
	stages     map[string]map[string][]byte
	nextID     int
	// pageSize is how many entries one allFiles response returns, so pagination is exercised
	// by default rather than only in a special test.
	pageSize int

	// hooks let a test interrupt the API at a specific moment.
	beforeCommit func(stageID string, names []string) error
	onListFiles  func()
}

type fakeContainer struct {
	objects   map[string]fakeObject
	versionID string
}

type fakeObject struct {
	content   []byte
	createdAt time.Time
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()

	api := &fakeAPI{
		containers: map[string]*fakeContainer{},
		stages:     map[string]map[string][]byte{},
		pageSize:   3,
	}

	api.server = httptest.NewServer(api.routes(t))
	t.Cleanup(api.server.Close)

	return api
}

// client returns a drapi client pointed at the fake.
func (a *fakeAPI) client(t *testing.T) *drapi.Client {
	t.Helper()

	client, err := drapi.New(drapi.Config{
		Endpoint:   a.server.URL + "/api/v2",
		Token:      "test-token",
		MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	return client
}

var (
	reAllFiles = regexp.MustCompile(`^/api/v2/files/([^/]+)/allFiles/$`)
	reStages   = regexp.MustCompile(`^/api/v2/files/([^/]+)/stages/$`)
	reUpload   = regexp.MustCompile(`^/api/v2/files/([^/]+)/stages/([^/]+)/upload/$`)
	reCommit   = regexp.MustCompile(`^/api/v2/files/([^/]+)/fromStage/$`)
	reLinks    = regexp.MustCompile(`^/api/v2/files/([^/]+)/links/$`)
	reDetails  = regexp.MustCompile(`^/api/v2/catalogItems/([^/]+)/extendedDetails/$`)
	reCatalog  = regexp.MustCompile(`^/api/v2/catalogItems/([^/]+)/$`)
	reBlob     = regexp.MustCompile(`^/blob/([^?]+)$`)
)

func (a *fakeAPI) routes(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// A presigned URL must work with no credentials, and must fail if any are sent —
		// which is what the real store does.
		if match := reBlob.FindStringSubmatch(path); match != nil {
			a.serveBlob(t, w, r, match[1])

			return
		}

		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)

			return
		}

		switch {
		case path == "/api/v2/files/" && r.Method == http.MethodPost:
			a.createContainer(t, w)
		case reAllFiles.MatchString(path) && r.Method == http.MethodGet:
			a.listFiles(t, w, r, reAllFiles.FindStringSubmatch(path)[1])
		case reAllFiles.MatchString(path) && r.Method == http.MethodDelete:
			a.deletePaths(t, w, r, reAllFiles.FindStringSubmatch(path)[1])
		case reStages.MatchString(path) && r.Method == http.MethodPost:
			a.createStage(t, w, reStages.FindStringSubmatch(path)[1])
		case reUpload.MatchString(path) && r.Method == http.MethodPost:
			match := reUpload.FindStringSubmatch(path)
			a.uploadToStage(t, w, r, match[1], match[2])
		case reCommit.MatchString(path) && r.Method == http.MethodPost:
			a.commitStage(t, w, r, reCommit.FindStringSubmatch(path)[1])
		case reLinks.MatchString(path) && r.Method == http.MethodPost:
			a.signLinks(t, w, r, reLinks.FindStringSubmatch(path)[1])
		case reDetails.MatchString(path) && r.Method == http.MethodGet:
			a.extendedDetails(t, w, reDetails.FindStringSubmatch(path)[1])
		case reCatalog.MatchString(path):
			a.catalogItem(t, w, r, reCatalog.FindStringSubmatch(path)[1])
		default:
			http.Error(w, fmt.Sprintf(`{"message":"no route for %s %s"}`, r.Method, path), http.StatusNotFound)
		}
	})
}

func (a *fakeAPI) createContainer(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	a.mu.Lock()
	a.nextID++
	id := fmt.Sprintf("container-%d", a.nextID)
	version := id + "-v0"
	a.containers[id] = &fakeContainer{objects: map[string]fakeObject{}, versionID: version}
	a.mu.Unlock()

	writeJSON(t, w, http.StatusOK, map[string]string{"catalogId": id, "catalogVersionId": version})
}

func (a *fakeAPI) listFiles(t *testing.T, w http.ResponseWriter, r *http.Request, id string) {
	t.Helper()

	if a.onListFiles != nil {
		a.onListFiles()
	}

	prefix := r.URL.Query().Get("prefix")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		writeJSON(t, w, http.StatusUnprocessableEntity,
			map[string]string{"message": `Prefix must end with a forward slash "/".`})

		return
	}

	a.mu.Lock()

	container, ok := a.containers[id]

	var names []string

	if ok {
		for name := range container.objects {
			if prefix == "" || strings.HasPrefix(name, prefix) {
				names = append(names, name)
			}
		}
	}

	objects := map[string]fakeObject{}

	if ok {
		for _, name := range names {
			objects[name] = container.objects[name]
		}
	}

	a.mu.Unlock()

	if !ok {
		writeJSON(t, w, http.StatusGone, map[string]string{"message": "Not Found"})

		return
	}

	sort.Strings(names)

	offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
	if err != nil {
		offset = 0
	}

	limit := a.pageSize
	if requested, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && requested > 0 {
		limit = min(requested, a.pageSize)
	}

	end := min(offset+limit, len(names))

	page := []map[string]any{}

	for _, name := range names[min(offset, len(names)):end] {
		object := objects[name]
		digest := sha256.Sum256(object.content)

		page = append(page, map[string]any{
			"fileName":      name,
			"fileType":      "xz",
			"fileSize":      len(object.content),
			"fileChecksum":  hex.EncodeToString(digest[:]),
			"ingestErrors":  "",
			"fileCreatedAt": object.createdAt.UTC().Format(time.RFC3339Nano),
		})
	}

	next := any(nil)
	if end < len(names) {
		next = fmt.Sprintf("%s/api/v2/files/%s/allFiles/?offset=%d&limit=%d&recursive=True&prefix=%s",
			a.server.URL, id, end, limit, prefix)
	}

	writeJSON(t, w, http.StatusOK, map[string]any{
		"count":      len(page),
		"totalCount": len(names),
		"next":       next,
		"previous":   nil,
		"data":       page,
	})
}

func (a *fakeAPI) createStage(t *testing.T, w http.ResponseWriter, id string) {
	t.Helper()

	a.mu.Lock()
	a.nextID++
	stageID := fmt.Sprintf("stage-%d", a.nextID)
	a.stages[stageID] = map[string][]byte{}
	a.mu.Unlock()

	writeJSON(t, w, http.StatusOK, map[string]string{"catalogId": id, "stageId": stageID})
}

func (a *fakeAPI) uploadToStage(t *testing.T, w http.ResponseWriter, r *http.Request, id, stageID string) {
	t.Helper()

	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, `{"message":"bad content type"}`, http.StatusBadRequest)

		return
	}

	reader := multipart.NewReader(r.Body, params["boundary"])

	part, err := reader.NextPart()
	if err != nil {
		http.Error(w, `{"message":"no part"}`, http.StatusBadRequest)

		return
	}

	// The full path lives in the raw Content-Disposition header; Go's FileName() would
	// basename it away, and the live API preserves the directory.
	name := rawFilename(part.Header.Get("Content-Disposition"))

	content, err := io.ReadAll(part)
	if err != nil {
		http.Error(w, `{"message":"unreadable part"}`, http.StatusBadRequest)

		return
	}

	a.mu.Lock()

	stage, ok := a.stages[stageID]
	if ok {
		// A repeated name inside one stage is deduplicated, last write winning.
		stage[name] = content
	}
	a.mu.Unlock()

	if !ok {
		writeJSON(t, w, http.StatusNotFound, map[string]string{"message": "no such stage"})

		return
	}

	writeJSON(t, w, http.StatusCreated, map[string]string{"catalogId": id, "stageId": stageID})
}

func (a *fakeAPI) commitStage(t *testing.T, w http.ResponseWriter, r *http.Request, id string) {
	t.Helper()

	var body struct {
		StageID   string `json:"stageId"`
		Overwrite string `json:"overwrite"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"message":"bad body"}`, http.StatusBadRequest)

		return
	}

	a.mu.Lock()

	stage, ok := a.stages[body.StageID]
	container, containerOK := a.containers[id]

	if !ok || !containerOK {
		a.mu.Unlock()
		writeJSON(t, w, http.StatusNotFound, map[string]string{"message": "no such stage or container"})

		return
	}

	names := make([]string, 0, len(stage))
	for name := range stage {
		names = append(names, name)
	}

	sort.Strings(names)
	a.mu.Unlock()

	if a.beforeCommit != nil {
		if err := a.beforeCommit(body.StageID, names); err != nil {
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]string{"message": err.Error()})

			return
		}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// overwrite=error rejects the entire commit if any name already exists, and applies none
	// of it.
	if body.Overwrite == "" || body.Overwrite == string(drapi.OverwriteError) {
		var duplicates []string

		for _, name := range names {
			if _, exists := container.objects[name]; exists {
				duplicates = append(duplicates, name)
			}
		}

		if len(duplicates) > 0 {
			writeJSON(t, w, http.StatusConflict, map[string]string{
				"message": "Duplicate file name(s): " + strings.Join(duplicates, ", "),
			})

			return
		}
	}

	now := time.Now()

	for name, content := range stage {
		container.objects[name] = fakeObject{content: content, createdAt: now}
	}

	delete(a.stages, body.StageID)

	a.nextID++
	container.versionID = fmt.Sprintf("%s-v%d", id, a.nextID)

	writeJSON(t, w, http.StatusOK, map[string]any{
		"catalogId":        id,
		"catalogVersionId": container.versionID,
		"numFiles":         len(container.objects),
	})
}

func (a *fakeAPI) signLinks(t *testing.T, w http.ResponseWriter, r *http.Request, id string) {
	t.Helper()

	var body struct {
		FileNames []string `json:"fileNames"`
		Duration  int      `json:"duration"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"message":"bad body"}`, http.StatusBadRequest)

		return
	}

	a.mu.Lock()
	container, ok := a.containers[id]

	var missing string

	if ok {
		for _, name := range body.FileNames {
			if _, exists := container.objects[name]; !exists {
				missing = name

				break
			}
		}
	}
	a.mu.Unlock()

	if !ok {
		writeJSON(t, w, http.StatusGone, map[string]string{"message": "Not Found"})

		return
	}

	// One missing name fails the whole batch, exactly as the live API does.
	if missing != "" {
		writeJSON(t, w, http.StatusUnprocessableEntity,
			map[string]string{"message": "Filename " + missing + " does not exist."})

		return
	}

	links := make([]map[string]string, 0, len(body.FileNames))

	for _, name := range body.FileNames {
		links = append(links, map[string]string{
			"fileName": name,
			"url":      fmt.Sprintf("%s/blob/%s?container=%s&sig=1", a.server.URL, name, id),
		})
	}

	writeJSON(t, w, http.StatusOK, map[string]any{"links": links})
}

func (a *fakeAPI) serveBlob(t *testing.T, w http.ResponseWriter, r *http.Request, name string) {
	t.Helper()

	// S3 answers 400 when a presigned signature and an Authorization header both appear.
	if r.Header.Get("Authorization") != "" {
		http.Error(w, "<Error><Code>InvalidArgument</Code></Error>", http.StatusBadRequest)

		return
	}

	id := r.URL.Query().Get("container")

	a.mu.Lock()
	container, ok := a.containers[id]

	var object fakeObject
	if ok {
		object, ok = container.objects[name]
	}
	a.mu.Unlock()

	if !ok {
		http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)

		return
	}

	if _, err := w.Write(object.content); err != nil {
		t.Errorf("writing blob: %v", err)
	}
}

func (a *fakeAPI) deletePaths(t *testing.T, w http.ResponseWriter, r *http.Request, id string) {
	t.Helper()

	var body struct {
		Paths []string `json:"paths"`
	}

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"message":"bad body"}`, http.StatusBadRequest)

		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	container, ok := a.containers[id]
	if !ok {
		writeJSON(t, w, http.StatusGone, map[string]string{"message": "Not Found"})

		return
	}

	results := make([]map[string]any, 0, len(body.Paths))

	for _, path := range body.Paths {
		removed := 0

		if _, exists := container.objects[path]; exists {
			delete(container.objects, path)

			removed = 1
		}

		results = append(results, map[string]any{"path": path, "numFilesDeleted": removed})
	}

	writeJSON(t, w, http.StatusOK, map[string]any{
		"catalogId":        id,
		"catalogVersionId": container.versionID,
		"numFiles":         len(container.objects),
		"results":          results,
	})
}

func (a *fakeAPI) extendedDetails(t *testing.T, w http.ResponseWriter, id string) {
	t.Helper()

	a.mu.Lock()
	container, ok := a.containers[id]

	version := ""
	if ok {
		version = container.versionID
	}
	a.mu.Unlock()

	if !ok {
		writeJSON(t, w, http.StatusGone, map[string]string{"message": "Not Found"})

		return
	}

	writeJSON(t, w, http.StatusOK, map[string]string{"id": id, "catalogVersionId": version})
}

func (a *fakeAPI) catalogItem(t *testing.T, w http.ResponseWriter, r *http.Request, id string) {
	t.Helper()

	if r.Method == http.MethodPatch {
		writeJSON(t, w, http.StatusOK, map[string]string{"id": id})

		return
	}

	writeJSON(t, w, http.StatusOK, map[string]string{"id": id, "catalogType": "files"})
}

// objectNames returns every object in a container, sorted.
func (a *fakeAPI) objectNames(containerID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	container, ok := a.containers[containerID]
	if !ok {
		return nil
	}

	names := make([]string, 0, len(container.objects))
	for name := range container.objects {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// countObjects returns how many objects match a prefix.
func (a *fakeAPI) countObjects(containerID, prefix string) int {
	count := 0

	for _, name := range a.objectNames(containerID) {
		if strings.HasPrefix(name, prefix) {
			count++
		}
	}

	return count
}

// setObjectAge backdates an object so a grace window can be tested without sleeping.
func (a *fakeAPI) setObjectAge(containerID, name string, age time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()

	container, ok := a.containers[containerID]
	if !ok {
		return
	}

	object, ok := container.objects[name]
	if !ok {
		return
	}

	object.createdAt = time.Now().Add(-age)
	container.objects[name] = object
}

// corruptObject overwrites an object's body with bytes that will fail to parse, so a test can
// exercise the "unreadable object" path without any real transport failure.
func (a *fakeAPI) corruptObject(containerID, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	container, ok := a.containers[containerID]
	if !ok {
		return
	}

	object, ok := container.objects[name]
	if !ok {
		return
	}

	object.content = []byte("not valid json")
	container.objects[name] = object
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	encoded, err := json.Marshal(body)
	if err != nil {
		t.Errorf("encoding fake response: %v", err)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if _, err := w.Write(encoded); err != nil {
		t.Errorf("writing fake response: %v", err)
	}
}

// rawFilename pulls the unmangled filename out of a Content-Disposition header.
func rawFilename(disposition string) string {
	const marker = `filename="`

	start := strings.Index(disposition, marker)
	if start < 0 {
		return ""
	}

	rest := disposition[start+len(marker):]

	var out strings.Builder

	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '\\':
			if i+1 < len(rest) {
				i++

				out.WriteByte(rest[i])
			}
		case '"':
			return out.String()
		default:
			out.WriteByte(rest[i])
		}
	}

	return out.String()
}
