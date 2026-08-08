package gomongoapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Shared MongoDB instance, backed by testcontainers, used by tests that need
// to exercise real database behavior. It is started once for the whole
// package in TestMain and reused by every test to keep the suite fast.
var (
	sharedMongoURI    string
	sharedMongoClient *mongo.Client
	sharedMongoErr    error
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(runTestMain(m))
}

// runTestMain starts the shared MongoDB container before running the test
// suite and tears it down afterwards. It is split out from TestMain so that
// deferred cleanup runs before the process exits via os.Exit.
func runTestMain(m *testing.M) int {
	ctx := context.Background()

	container, err := mongodb.Run(ctx, "mongo:6")
	if err != nil {
		sharedMongoErr = err
		return m.Run()
	}
	defer func() { _ = container.Terminate(ctx) }()

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		sharedMongoErr = err
		return m.Run()
	}

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		sharedMongoErr = err
		return m.Run()
	}
	defer func() { _ = client.Disconnect(ctx) }()

	if err := client.Ping(ctx, nil); err != nil {
		sharedMongoErr = err
		return m.Run()
	}

	sharedMongoURI = uri
	sharedMongoClient = client

	return m.Run()
}

// requireMongo skips the calling test when the shared MongoDB container
// could not be started (for example, when Docker isn't available).
func requireMongo(t *testing.T) *mongo.Client {
	t.Helper()

	if sharedMongoErr != nil {
		t.Skipf("mongodb test container unavailable: %v", sharedMongoErr)
	}

	return sharedMongoClient
}

// disconnectedClient returns a Mongo client connected to the shared
// container and then immediately disconnected, so that any operation run
// against it fails. This is used to exercise the handlers' error branches
// without needing to mock the driver.
func disconnectedClient(t *testing.T) *mongo.Client {
	t.Helper()
	requireMongo(t)

	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(sharedMongoURI))
	require.NoError(t, err)
	require.NoError(t, client.Disconnect(ctx))

	return client
}

// maxMongoDBNameLen is MongoDB's limit on database name length: names must
// be fewer than 64 characters.
const maxMongoDBNameLen = 63

// testDBName derives a unique, valid Mongo database name from the current
// test name so tests sharing the container don't collide with each other.
func testDBName(t *testing.T) string {
	t.Helper()
	replacer := strings.NewReplacer("/", "_", " ", "_")
	name := "test_" + replacer.Replace(t.Name())
	if len(name) <= maxMongoDBNameLen {
		return name
	}

	// Long test names (especially subtests) can exceed Mongo's database name
	// limit. Truncate and append a short hash of the full name so distinct
	// long names don't collide once truncated.
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	suffix := fmt.Sprintf("_%08x", h.Sum32())
	return name[:maxMongoDBNameLen-len(suffix)] + suffix
}

func TestTestDBName_StaysWithinMongoLimit(t *testing.T) {
	shortName := testDBName(t)
	assert.LessOrEqual(t, len(shortName), maxMongoDBNameLen)

	t.Run("ThisIsADeliberatelyVeryLongSubtestNameChosenToExceedMongoDBsSixtyFourCharacterDatabaseNameLimitOnItsOwn", func(t *testing.T) {
		longName := testDBName(t)
		assert.LessOrEqual(t, len(longName), maxMongoDBNameLen)
		assert.True(t, strings.HasPrefix(longName, "test_"))
	})
}

// newTestServer builds a server directly (bypassing NewServer/Options) so
// tests can inject a specific Mongo client and limits. Callers are
// responsible for calling createRoutes() once any middleware/custom routes
// have been configured, mirroring the order NewServer/Start use in
// production: middleware and custom routes are set by the caller before
// Start() wires up the api/custom routes.
func newTestServer(client *mongo.Client, defaultDB string, findLimit, findMaxLimit int) *server {
	router := gin.New()
	return &server{
		router:             router,
		apiRouter:          router.Group("/api"),
		customRouter:       router.Group("custom"),
		mongoClient:        client,
		defaultDB:          defaultDB,
		findLimit:          strconv.Itoa(findLimit),
		maxLimit:           findMaxLimit,
		healthCheckTimeout: defaultHealthCheckTimeout,
	}
}

// captureLog redirects the standard logger's output to a buffer for the
// duration of the test, restoring it on cleanup, so tests can assert on
// warnings logged via the log package.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })

	return &buf
}

// doJSONRequest issues a request with a JSON content type.
func doJSONRequest(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

// errorBody decodes a handler failure response as the {"error": "..."} JSON
// envelope and returns the message, failing the test if the body isn't in
// that shape.
func errorBody(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()

	assert.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))

	var body struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.NotEmpty(t, body.Error)
	return body.Error
}

// ---- NewServer ----

func TestNewServer(t *testing.T) {
	opts := ServerOptions()
	opts.SetAddress(":9999")
	opts.SetDefaultDB("app")
	opts.SetFindLimit(50)
	opts.SetFindMaxLimit(500)
	opts.SetConnectTimeout(20 * time.Second)
	opts.SetShutdownTimeout(15 * time.Second)
	opts.SetReadHeaderTimeout(3 * time.Second)
	opts.SetHealthCheckTimeout(2 * time.Second)
	opts.SetAllowUnsafeOperators(true)
	require.NoError(t, opts.SetCustomRouteName("myroutes"))

	srv := NewServer(opts)
	require.NotNil(t, srv)

	s, ok := srv.(*server)
	require.True(t, ok)

	assert.Same(t, opts.Router, s.router)
	assert.Same(t, opts.MongoClientOpts, s.mongoClientOpts)
	assert.Equal(t, ":9999", s.address)
	assert.Equal(t, "app", s.defaultDB)
	assert.Equal(t, "50", s.findLimit)
	assert.Equal(t, 500, s.maxLimit)
	assert.Equal(t, 20*time.Second, s.connectTimeout)
	assert.Equal(t, 15*time.Second, s.shutdownTimeout)
	assert.Equal(t, 3*time.Second, s.readHeaderTimeout)
	assert.Equal(t, 2*time.Second, s.healthCheckTimeout)
	assert.True(t, s.allowUnsafeOperators)
	require.NotNil(t, s.apiRouter)
	require.NotNil(t, s.customRouter)
	assert.Equal(t, "/api", s.apiRouter.BasePath())
	assert.Equal(t, "/myroutes", s.customRouter.BasePath())
}

func TestNewServer_NilRouter(t *testing.T) {
	opts := ServerOptions()
	opts.Router = nil

	require.NotPanics(t, func() {
		srv := NewServer(opts)
		require.NotNil(t, srv)

		s, ok := srv.(*server)
		require.True(t, ok)

		assert.Nil(t, s.router)
		assert.Nil(t, s.apiRouter)
		assert.Nil(t, s.customRouter)
	})
}

// ---- createRoutes / root route ----

func TestServer_RootRoute(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

// ---- /api/health ----

func TestHealth_Success(t *testing.T) {
	client := requireMongo(t)

	s := newTestServer(client, "", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body struct{ Status string }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "ok", body.Status)
}

func TestHealth_MongoUnreachable(t *testing.T) {
	client := disconnectedClient(t)

	s := newTestServer(client, "", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, errorBody(t, w), "Error pinging MongoDB")
}

// ---- Middleware / custom routes ----

func TestSetAPIMiddleware(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.SetAPIMiddleware(func(c *gin.Context) {
		c.Header("X-Test-Middleware", "hit")
	})
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/databases", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get("X-Test-Middleware"))
}

func TestSetAPIKey_MissingOrWrongKeyRejected(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.SetAPIKey("correct-key")
	s.createRoutes()

	tests := []struct {
		name   string
		header string
	}{
		{name: "no header", header: ""},
		{name: "wrong key", header: "wrong-key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/databases", nil)
			if tt.header != "" {
				req.Header.Set("X-API-Key", tt.header)
			}
			s.router.ServeHTTP(w, req)

			assert.Equal(t, http.StatusUnauthorized, w.Code)
		})
	}
}

func TestSetAPIKey_CorrectKeyAccepted(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.SetAPIKey("correct-key")
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/databases", nil)
	req.Header.Set("X-API-Key", "correct-key")
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestSetCustomMiddleware(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.SetCustomMiddleware(func(c *gin.Context) {
		c.Header("X-Test-Middleware", "hit")
	})
	s.AddCustomGET("/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/custom/ping", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "hit", w.Header().Get("X-Test-Middleware"))
}

func TestSetAPIMiddleware_AfterRoutesRegisteredLogsWarning(t *testing.T) {
	buf := captureLog(t)

	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	s.SetAPIMiddleware(func(_ *gin.Context) {})

	assert.Contains(t, buf.String(), "SetAPIMiddleware called after Start()")
}

func TestSetCustomMiddleware_AfterRoutesRegisteredLogsWarning(t *testing.T) {
	buf := captureLog(t)

	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	s.SetCustomMiddleware(func(_ *gin.Context) {})

	assert.Contains(t, buf.String(), "SetCustomMiddleware called after Start()")
}

func TestSetAPIMiddleware_BeforeRoutesRegisteredNoWarning(t *testing.T) {
	buf := captureLog(t)

	s := newTestServer(nil, "app", 100, 0)
	s.SetAPIMiddleware(func(_ *gin.Context) {})
	s.createRoutes()

	assert.Empty(t, buf.String())
}

func TestAddCustomPOST(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.AddCustomPOST("/echo", func(c *gin.Context) {
		var body map[string]any
		require.NoError(t, c.ShouldBindJSON(&body))
		c.JSON(http.StatusOK, body)
	})
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/custom/echo", `{"hello":"world"}`)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"hello":"world"}`, w.Body.String())
}

// ---- GetMongoClient ----

func TestGetMongoClient(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	assert.Nil(t, s.GetMongoClient())

	client := requireMongo(t)
	s.mongoClient = client
	assert.Same(t, client, s.GetMongoClient())
}

// ---- Start ----

func TestStart_InvalidMongoOptions(t *testing.T) {
	opts := ServerOptions()
	opts.SetMongoClientOpts(options.Client().ApplyURI("not-a-valid-mongodb-uri"))

	srv := NewServer(opts)
	err := srv.Start()

	require.Error(t, err)
}

func TestStart_PingFailure(t *testing.T) {
	opts := ServerOptions()
	opts.SetMongoClientOpts(
		options.Client().
			ApplyURI("mongodb://127.0.0.1:1").
			SetServerSelectionTimeout(200 * time.Millisecond).
			SetConnectTimeout(200 * time.Millisecond),
	)

	srv := NewServer(opts)
	err := srv.Start()

	require.Error(t, err)
}

func TestStart_NilRouter(t *testing.T) {
	requireMongo(t)

	opts := ServerOptions()
	opts.SetMongoClientOpts(options.Client().ApplyURI(sharedMongoURI))
	opts.Router = nil

	srv := NewServer(opts)
	err := srv.Start()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "gin router")
}

func TestStart_Success(t *testing.T) {
	requireMongo(t)

	opts := ServerOptions()
	opts.SetMongoClientOpts(options.Client().ApplyURI(sharedMongoURI))
	opts.SetAddress("127.0.0.1:0")

	srv := NewServer(opts)

	errCh := make(chan error, 1)
	go func() {
		// Start() blocks serving HTTP until a shutdown signal arrives or an
		// error occurs.
		errCh <- srv.Start()
	}()

	select {
	case err := <-errCh:
		t.Fatalf("Start() returned unexpectedly: %v", err)
	case <-time.After(300 * time.Millisecond):
		client := srv.GetMongoClient()
		require.NotNil(t, client)
		assert.NoError(t, client.Ping(context.Background(), nil))
	}

	// Signal the running server so its goroutine doesn't outlive the test;
	// TestStart_GracefulShutdown covers the shutdown behavior itself.
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after SIGTERM")
	}
}

func TestStart_GracefulShutdown(t *testing.T) {
	requireMongo(t)

	opts := ServerOptions()
	opts.SetMongoClientOpts(options.Client().ApplyURI(sharedMongoURI))
	opts.SetAddress("127.0.0.1:0")

	srv := NewServer(opts)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	// Give Start() time to connect, register its SIGTERM handler, and start
	// serving before signalling shutdown.
	select {
	case err := <-errCh:
		t.Fatalf("Start() returned before shutdown was requested: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	client := srv.GetMongoClient()
	require.NotNil(t, client)
	require.NoError(t, client.Ping(context.Background(), nil))

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))

	select {
	case err := <-errCh:
		assert.NoError(t, err, "Start() should return nil after a graceful shutdown")
	case <-time.After(5 * time.Second):
		t.Fatal("Start() did not return after SIGTERM")
	}

	// The deferred Mongo disconnect should have run as part of shutdown.
	assert.Error(t, client.Ping(context.Background(), nil))
}

func TestStart_ConnectTimeout(t *testing.T) {
	// 192.0.2.0/24 is reserved for documentation (RFC 5737) and never
	// responds, so Connect/Ping only return once our own connectTimeout
	// fires rather than because of a connection refusal.
	opts := ServerOptions()
	opts.SetMongoClientOpts(
		options.Client().
			ApplyURI("mongodb://192.0.2.1:27017").
			SetServerSelectionTimeout(30 * time.Second).
			SetConnectTimeout(30 * time.Second),
	)
	opts.SetConnectTimeout(300 * time.Millisecond)

	srv := NewServer(opts)

	start := time.Now()
	err := srv.Start()
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second, "Start() should have aborted via Options.ConnectTimeout, not the driver's own 30s timeout")
}

// ---- getDatabases ----

func TestGetDatabases_DefaultDB(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/databases", nil)
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body struct{ Databases []string }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, []string{"app"}, body.Databases)
}

func TestGetDatabases_ListsFromMongo(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	_, err := client.Database(dbName).Collection("seed").InsertOne(ctx, bson.M{"a": 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, "", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/databases", nil)
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body struct{ Databases []string }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.Databases, dbName)
}

func TestGetDatabases_MongoError(t *testing.T) {
	client := disconnectedClient(t)

	s := newTestServer(client, "", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/databases", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, errorBody(t, w), "Error getting databases names")
}

// ---- getCollections ----

func TestGetCollections_MissingDatabaseParam(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/collections", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Database name was not passed, one is needed", errorBody(t, w))
}

func TestGetCollections_UsesDefaultDB(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	_, err := client.Database(dbName).Collection("widgets").InsertOne(ctx, bson.M{"a": 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/collections", nil)
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body struct{ Collections []string }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.Collections, "widgets")
}

func TestGetCollections_UsesQueryParam(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	_, err := client.Database(dbName).Collection("gadgets").InsertOne(ctx, bson.M{"a": 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, "", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/collections?database="+dbName, nil)
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var body struct{ Collections []string }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.Collections, "gadgets")
}

func TestGetCollections_MongoError(t *testing.T) {
	client := disconnectedClient(t)

	s := newTestServer(client, "app", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/collections", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, errorBody(t, w), "Error getting collection names")
}

// ---- collectionFind ----

func TestCollectionFind_MissingDatabaseParam(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Database name was not passed, one is needed", errorBody(t, w))
}

func TestCollectionFind_MissingCollectionName(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "name", Value: ""}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/collections//find", bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	s.collectionFind(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Collection name was not passed", errorBody(t, w))
}

func TestCollectionFind_BadLimit(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find?limit=notanumber", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), "Limit is not an int")
}

func TestCollectionFind_LimitExceedsMax(t *testing.T) {
	s := newTestServer(nil, "app", 100, 10)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find?limit=50", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Passed limit is greater than max limit set by server", errorBody(t, w))
}

func TestCollectionFind_DefaultLimitExceedsMax_Clamped(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	docs := make([]any, 0, 5)
	for i := range 5 {
		docs = append(docs, bson.M{"n": i})
	}
	_, err := coll.InsertMany(ctx, docs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	// findLimit (1000) exceeds maxLimit (2); a request with no explicit "limit"
	// must succeed and be capped at maxLimit rather than 400ing.
	s := newTestServer(client, dbName, 1000, 2)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", `{}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	assert.Len(t, results, 2)
}

func TestCollectionFind_BadRequestBody(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", `not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), "Error reading body request")
}

func TestCollectionFind_EmptyBodyUsesEmptyFilter(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a"},
		bson.M{"name": "b"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", "")
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	assert.Len(t, results, 2)
}

func TestCollectionFind_Success(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a", "qty": 1},
		bson.M{"name": "b", "qty": 2},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", `{"name":"a"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	require.Len(t, results, 1)
	assert.Equal(t, "a", results[0]["name"])
}

func TestCollectionFind_RespectsLimit(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	docs := make([]any, 0, 5)
	for i := range 5 {
		docs = append(docs, bson.M{"n": i})
	}
	_, err := coll.InsertMany(ctx, docs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find?limit=2", `{}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	assert.Len(t, results, 2)
}

func TestCollectionFind_MongoError(t *testing.T) {
	client := disconnectedClient(t)

	s := newTestServer(client, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", `{}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, errorBody(t, w), "Error running find")
}

func TestParseSortParam(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    bson.D
		wantErr bool
	}{
		{name: "single field ascending", raw: `{"createdAt":1}`, want: bson.D{{Key: "createdAt", Value: 1}}},
		{name: "single field descending", raw: `{"createdAt":-1}`, want: bson.D{{Key: "createdAt", Value: -1}}},
		{name: "multi field preserves order", raw: `{"a":1,"b":-1}`, want: bson.D{{Key: "a", Value: 1}, {Key: "b", Value: -1}}},
		{name: "empty object", raw: `{}`, want: nil},
		{name: "not json", raw: `notjson`, wantErr: true},
		{name: "not an object", raw: `[1,2]`, wantErr: true},
		{name: "invalid direction", raw: `{"a":2}`, wantErr: true},
		{name: "non numeric direction", raw: `{"a":"asc"}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSortParam(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// ---- findBannedOperator ----

func TestFindBannedOperator(t *testing.T) {
	tests := []struct {
		name    string
		v       any
		wantOp  string
		wantHit bool
	}{
		{name: "empty filter", v: map[string]any{}, wantHit: false},
		{name: "safe filter", v: map[string]any{"name": "a", "$or": []any{map[string]any{"qty": 1}}}, wantHit: false},
		{name: "top-level $where", v: map[string]any{"$where": "this.a == this.b"}, wantOp: "$where", wantHit: true},
		{name: "nested $function inside $expr", v: map[string]any{"$expr": map[string]any{"$function": "..."}}, wantOp: "$function", wantHit: true},
		{name: "$out nested in array stage", v: []any{map[string]any{"$match": map[string]any{}}, map[string]any{"$out": "otherColl"}}, wantOp: "$out", wantHit: true},
		{name: "$merge stage", v: []any{map[string]any{"$merge": map[string]any{"into": "otherColl"}}}, wantOp: "$merge", wantHit: true},
		{name: "banned op buried in $and", v: map[string]any{"$and": []any{map[string]any{"a": 1}, map[string]any{"$where": "1"}}}, wantOp: "$where", wantHit: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op, found := findBannedOperator(tt.v)
			assert.Equal(t, tt.wantHit, found)
			if tt.wantHit {
				assert.Equal(t, tt.wantOp, op)
			}
		})
	}
}

// ---- hasWriteAction ----

func TestHasWriteAction(t *testing.T) {
	tests := []struct {
		name       string
		privileges []connectionStatusPrivilege
		wantAction string
		wantFound  bool
	}{
		{name: "no privileges", privileges: nil, wantFound: false},
		{name: "read-only actions", privileges: []connectionStatusPrivilege{{Actions: []string{"find", "listCollections"}}}, wantFound: false},
		{name: "insert action", privileges: []connectionStatusPrivilege{{Actions: []string{"find", "insert"}}}, wantAction: "insert", wantFound: true},
		{name: "update action in second privilege", privileges: []connectionStatusPrivilege{{Actions: []string{"find"}}, {Actions: []string{"update"}}}, wantAction: "update", wantFound: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, found := hasWriteAction(tt.privileges)
			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				assert.Equal(t, tt.wantAction, action)
			}
		})
	}
}

// ---- warnIfWritableCredentials ----

func TestWarnIfWritableCredentials_NoAuthDoesNotWarn(t *testing.T) {
	// The shared test container runs without auth configured, so
	// connectionStatus reports no authenticated user and therefore no
	// privileges to flag; the call must not panic or log a warning.
	client := requireMongo(t)
	buf := captureLog(t)

	s := newTestServer(client, "", 100, 0)
	s.warnIfWritableCredentials(context.Background())

	assert.Empty(t, buf.String())
}

func TestWarnIfWritableCredentials_WritableUserWarns(t *testing.T) {
	ctx := context.Background()

	container, err := mongodb.Run(ctx, "mongo:6", mongodb.WithUsername("root"), mongodb.WithPassword("password"))
	if err != nil {
		t.Skipf("mongodb test container unavailable: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	uri, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Disconnect(ctx) })
	require.NoError(t, client.Ping(ctx, nil))

	buf := captureLog(t)

	s := newTestServer(client, "", 100, 0)
	s.warnIfWritableCredentials(ctx)

	assert.Contains(t, buf.String(), "write privileges")
}

// ---- AllowUnsafeOperators ----

func TestCollectionFind_RejectsUnsafeOperator(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", `{"$where":"this.a == this.b"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), `disallowed operator "$where"`)
}

func TestCollectionFind_AllowUnsafeOperatorsPermitsThem(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertOne(ctx, bson.M{"a": 1, "b": 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.allowUnsafeOperators = true
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", `{"$where":"this.a == this.b"}`)
	require.Equal(t, http.StatusOK, w.Code)
}

func TestCollectionCount_RejectsUnsafeOperator(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/count", `{"$where":"this.a == this.b"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), `disallowed operator "$where"`)
}

func TestCollectionAggregate_RejectsOutStage(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{"Aggregate":[{"$out":"otherColl"}]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), `disallowed stage/operator "$out"`)
}

func TestCollectionAggregate_RejectsMergeStage(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	body := `{"Aggregate":[{"$merge":{"into":"otherColl"}}]}`
	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), `disallowed stage/operator "$merge"`)
}

func TestCollectionAggregate_RejectsNestedWhereOperator(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	body := `{"Aggregate":[{"$match":{"$where":"this.a == this.b"}}]}`
	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), `disallowed stage/operator "$where"`)
}

func TestCollectionAggregate_AllowUnsafeOperatorsPermitsOutStage(t *testing.T) {
	// Also a regression test for endsInOutOrMerge: appending the server's
	// trailing $limit stage after a caller's $out would be an invalid
	// pipeline, since Mongo requires $out to be the terminal stage.
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertOne(ctx, bson.M{"name": "a"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.allowUnsafeOperators = true
	s.createRoutes()

	body := `{"Aggregate":[{"$out":"widgetsCopy"}]}`
	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", body)
	require.Equal(t, http.StatusOK, w.Code)
	t.Cleanup(func() { _ = client.Database(dbName).Collection("widgetsCopy").Drop(context.Background()) })

	count, err := client.Database(dbName).Collection("widgetsCopy").CountDocuments(ctx, bson.M{})
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
}

func TestEndsInOutOrMerge(t *testing.T) {
	tests := []struct {
		name     string
		pipeline []any
		want     bool
	}{
		{name: "empty pipeline", pipeline: []any{}, want: false},
		{name: "match only", pipeline: []any{map[string]any{"$match": map[string]any{}}}, want: false},
		{name: "ends in $out", pipeline: []any{map[string]any{"$match": map[string]any{}}, map[string]any{"$out": "coll"}}, want: true},
		{name: "ends in $merge", pipeline: []any{map[string]any{"$merge": map[string]any{"into": "coll"}}}, want: true},
		{name: "$out not last stage", pipeline: []any{map[string]any{"$out": "coll"}, map[string]any{"$match": map[string]any{}}}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, endsInOutOrMerge(tt.pipeline))
		})
	}
}

func TestCollectionFind_BadSkip(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find?skip=notanumber", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), "Skip is not an int")
}

func TestCollectionFind_NegativeSkip(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find?skip=-1", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Skip must be a non-negative int", errorBody(t, w))
}

func TestCollectionFind_BadSort(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find?sort=notjson", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), "Invalid sort parameter")
}

func TestCollectionFind_ProjectionFieldNotObject(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", `{"projection":"notanobject"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Projection field must be an object", errorBody(t, w))
}

func TestCollectionFind_RespectsSort(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"n": 3},
		bson.M{"n": 1},
		bson.M{"n": 2},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	path := "/api/collections/widgets/find?sort=" + url.QueryEscape(`{"n":1}`)
	w := doJSONRequest(s.router, http.MethodPost, path, `{}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	require.Len(t, results, 3)
	assert.EqualValues(t, 1, results[0]["n"])
	assert.EqualValues(t, 2, results[1]["n"])
	assert.EqualValues(t, 3, results[2]["n"])
}

func TestCollectionFind_MultiFieldSortOrderPreserved(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"group": "b", "n": 1},
		bson.M{"group": "a", "n": 2},
		bson.M{"group": "a", "n": 1},
		bson.M{"group": "b", "n": 2},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	// Sort by group ascending, then n descending within each group. This is
	// only correct if the sort key order from the JSON object is preserved.
	path := "/api/collections/widgets/find?sort=" + url.QueryEscape(`{"group":1,"n":-1}`)
	w := doJSONRequest(s.router, http.MethodPost, path, `{}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	require.Len(t, results, 4)
	assert.Equal(t, "a", results[0]["group"])
	assert.EqualValues(t, 2, results[0]["n"])
	assert.Equal(t, "a", results[1]["group"])
	assert.EqualValues(t, 1, results[1]["n"])
	assert.Equal(t, "b", results[2]["group"])
	assert.EqualValues(t, 2, results[2]["n"])
	assert.Equal(t, "b", results[3]["group"])
	assert.EqualValues(t, 1, results[3]["n"])
}

func TestCollectionFind_RespectsSkip(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	docs := make([]any, 0, 5)
	for i := range 5 {
		docs = append(docs, bson.M{"n": i})
	}
	_, err := coll.InsertMany(ctx, docs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	path := "/api/collections/widgets/find?skip=2&sort=" + url.QueryEscape(`{"n":1}`)
	w := doJSONRequest(s.router, http.MethodPost, path, `{}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	require.Len(t, results, 3)
	assert.EqualValues(t, 2, results[0]["n"])
	assert.EqualValues(t, 3, results[1]["n"])
	assert.EqualValues(t, 4, results[2]["n"])
}

func TestCollectionFind_RespectsProjection(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a", "qty": 5, "secret": "x"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	body := `{"name":"a","projection":{"name":1,"qty":1,"_id":0}}`
	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", body)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	require.Len(t, results, 1)
	assert.Equal(t, map[string]any{"name": "a", "qty": float64(5)}, results[0])
}

func TestCollectionFind_UppercaseProjectionKeyIsApplied(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a", "qty": 5, "secret": "x"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	body := `{"name":"a","Projection":{"name":1,"_id":0}}`
	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", body)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	require.Len(t, results, 1)
	assert.Equal(t, map[string]any{"name": "a"}, results[0])
}

// ---- collectionCount ----

func TestCollectionCount_MissingDatabaseParam(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/count", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Database name was not passed, one is needed", errorBody(t, w))
}

func TestCollectionCount_MissingCollectionName(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "name", Value: ""}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/collections//count", bytes.NewBufferString(`{}`))
	c.Request.Header.Set("Content-Type", "application/json")

	s.collectionCount(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Collection name was not passed", errorBody(t, w))
}

// TestCollectionCount_BodyTooLarge exercises the MaxBodyBytes limit end to
// end via NewServer, since newTestServer builds a *server directly and
// bypasses the middleware NewServer registers on apiRouter.
func TestCollectionCount_BodyTooLarge(t *testing.T) {
	opts := ServerOptions()
	opts.SetRouter(gin.New())
	opts.SetDefaultDB("app")
	opts.SetMaxBodyBytes(10)

	s, ok := NewServer(opts).(*server)
	require.True(t, ok)
	s.createRoutes()

	oversizedBody := fmt.Sprintf(`{"filter": "%s"}`, strings.Repeat("a", 100))
	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/count", oversizedBody)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
	assert.Contains(t, errorBody(t, w), "too large")
}

func TestCollectionCount_BadRequestBody(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/count", `not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), "Error reading body request")
}

func TestCollectionCount_EmptyBodyUsesEmptyFilter(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a"},
		bson.M{"name": "b"},
		bson.M{"name": "c"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/count", "")
	require.Equal(t, http.StatusOK, w.Code)

	var body struct{ Count int64 }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.EqualValues(t, 3, body.Count)
}

func TestCollectionCount_Success(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a"},
		bson.M{"name": "a"},
		bson.M{"name": "b"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/count", `{"name":"a"}`)
	require.Equal(t, http.StatusOK, w.Code)

	var body struct{ Count int64 }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.EqualValues(t, 2, body.Count)
}

func TestCollectionCount_MongoError(t *testing.T) {
	client := disconnectedClient(t)

	s := newTestServer(client, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/count", `{}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, errorBody(t, w), "Error running find")
}

// ---- collectionAggregate ----

func TestCollectionAggregate_MissingDatabaseParam(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{"Aggregate":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Database name was not passed, one is needed", errorBody(t, w))
}

func TestCollectionAggregate_MissingCollectionName(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "name", Value: ""}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/collections//aggregate", bytes.NewBufferString(`{"Aggregate":[]}`))
	c.Request.Header.Set("Content-Type", "application/json")

	s.collectionAggregate(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Collection name was not passed", errorBody(t, w))
}

func TestCollectionAggregate_BadRequestBody(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), "Error reading body request")
}

func TestCollectionAggregate_NonArrayAggregateField(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{"Aggregate":"not-an-array"}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Aggregate field must be an array", errorBody(t, w))
}

func TestCollectionAggregate_MissingAggregateFieldUsesEmptyPipeline(t *testing.T) {
	// Deliberate behavior: a body with no recognizable pipeline field (under any
	// casing of "aggregate") stays a 200 with an empty pipeline rather than a 400,
	// so callers can intentionally fetch an entire small collection without
	// constructing a no-op pipeline.
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertOne(ctx, bson.M{"name": "a"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	assert.Len(t, results, 1)
}

func TestCollectionAggregate_EmptyBodyUsesEmptyPipeline(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertOne(ctx, bson.M{"name": "a"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", "")
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	assert.Len(t, results, 1)
}

func TestCollectionAggregate_AcceptsJSONWithoutContentTypeHeader(t *testing.T) {
	// Regression test: collectionAggregate previously used ctx.ShouldBind,
	// which selects a binder from Content-Type and fell through to form
	// binding when the header was absent, failing on a well-formed JSON body.
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a", "qty": 1},
		bson.M{"name": "b", "qty": 2},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/collections/widgets/aggregate",
		bytes.NewBufferString(`{"Aggregate":[{"$match":{"name":"a"}}]}`))
	// Deliberately no Content-Type header set.
	s.router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	require.Len(t, results, 1)
	assert.Equal(t, "a", results[0]["name"])
}

func TestCollectionAggregate_LowercaseAggregateKeyIsApplied(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a", "qty": 1},
		bson.M{"name": "a", "qty": 3},
		bson.M{"name": "b", "qty": 5},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	body := `{"aggregate":[{"$match":{"name":"a"}},{"$group":{"_id":"$name","total":{"$sum":"$qty"}}}]}`
	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", body)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	require.Len(t, results, 1)
	assert.EqualValues(t, 4, results[0]["total"])
}

func TestCollectionAggregate_BadLimit(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate?limit=notanumber", `{"Aggregate":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, errorBody(t, w), "Limit is not an int")
}

func TestCollectionAggregate_LimitExceedsMax(t *testing.T) {
	s := newTestServer(nil, "app", 100, 10)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate?limit=50", `{"Aggregate":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Passed limit is greater than max limit set by server", errorBody(t, w))
}

func TestCollectionAggregate_DefaultLimitExceedsMax_Clamped(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	docs := make([]any, 0, 5)
	for i := range 5 {
		docs = append(docs, bson.M{"n": i})
	}
	_, err := coll.InsertMany(ctx, docs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	// findLimit (1000) exceeds maxLimit (2); a request with no explicit "limit"
	// must succeed and be capped at maxLimit rather than 400ing.
	s := newTestServer(client, dbName, 1000, 2)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{"Aggregate":[]}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	assert.Len(t, results, 2)
}

func TestCollectionAggregate_RespectsDefaultLimit(t *testing.T) {
	// An empty pipeline would otherwise return the entire collection; the
	// server-wide default limit must still cap it.
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	docs := make([]any, 0, 5)
	for i := range 5 {
		docs = append(docs, bson.M{"n": i})
	}
	_, err := coll.InsertMany(ctx, docs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 2, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{"Aggregate":[]}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	assert.Len(t, results, 2)
}

func TestCollectionAggregate_RespectsLimitParam(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	docs := make([]any, 0, 5)
	for i := range 5 {
		docs = append(docs, bson.M{"n": i})
	}
	_, err := coll.InsertMany(ctx, docs)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate?limit=3", `{"Aggregate":[]}`)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	assert.Len(t, results, 3)
}

func TestCollectionAggregate_LimitAppliedAfterGroupStage(t *testing.T) {
	// Verifies the trailing $limit stage still caps results even when the
	// caller's pipeline ends in a $group, which produces fewer documents
	// than it consumed.
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a", "qty": 1},
		bson.M{"name": "b", "qty": 2},
		bson.M{"name": "c", "qty": 3},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 2, 0)
	s.createRoutes()

	body := `{"Aggregate":[{"$group":{"_id":"$name","total":{"$sum":"$qty"}}}]}`
	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", body)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	assert.Len(t, results, 2)
}

func TestCollectionAggregate_NonArrayLowercaseAggregateField(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{"aggregate":"not-an-array"}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "Aggregate field must be an array", errorBody(t, w))
}

func TestCollectionAggregate_Success(t *testing.T) {
	client := requireMongo(t)
	dbName := testDBName(t)
	ctx := context.Background()

	coll := client.Database(dbName).Collection("widgets")
	_, err := coll.InsertMany(ctx, []any{
		bson.M{"name": "a", "qty": 1},
		bson.M{"name": "a", "qty": 3},
		bson.M{"name": "b", "qty": 5},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Database(dbName).Drop(context.Background()) })

	s := newTestServer(client, dbName, 100, 0)
	s.createRoutes()

	body := `{"Aggregate":[{"$match":{"name":"a"}},{"$group":{"_id":"$name","total":{"$sum":"$qty"}}}]}`
	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", body)
	require.Equal(t, http.StatusOK, w.Code)

	var results []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &results))
	require.Len(t, results, 1)
	assert.EqualValues(t, 4, results[0]["total"])
}

func TestCollectionAggregate_MongoError(t *testing.T) {
	client := disconnectedClient(t)

	s := newTestServer(client, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{"Aggregate":[]}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, errorBody(t, w), "Error running aggregate")
}
