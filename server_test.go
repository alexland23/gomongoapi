package gomongoapi

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
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
	sum := sha1.Sum([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:])[:8]
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
		router:       router,
		apiRouter:    router.Group("/api"),
		customRouter: router.Group("custom"),
		mongoClient:  client,
		defaultDB:    defaultDB,
		findLimit:    strconv.Itoa(findLimit),
		maxLimit:     findMaxLimit,
	}
}

// doJSONRequest issues a request with a JSON content type, which the
// aggregate route's ctx.ShouldBind depends on to select JSON binding.
func doJSONRequest(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, req)
	return w
}

// ---- NewServer ----

func TestNewServer(t *testing.T) {
	opts := ServerOptions()
	opts.SetAddress(":9999")
	opts.SetDefaultDB("app")
	opts.SetFindLimit(50)
	opts.SetFindMaxLimit(500)
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
		// Start() blocks serving HTTP until an error occurs; there's no
		// graceful-shutdown hook on the Server interface, so this goroutine
		// is intentionally left running for the rest of the test binary.
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
}

// ---- getCollections ----

func TestGetCollections_MissingDatabaseParam(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/collections", nil)
	s.router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
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
}

// ---- collectionFind ----

func TestCollectionFind_MissingDatabaseParam(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
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
	assert.Contains(t, w.Body.String(), "Collection name was not passed")
}

func TestCollectionFind_BadLimit(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find?limit=notanumber", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCollectionFind_LimitExceedsMax(t *testing.T) {
	s := newTestServer(nil, "app", 100, 10)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/find?limit=50", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
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
}

// ---- collectionCount ----

func TestCollectionCount_MissingDatabaseParam(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/count", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
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
	assert.Contains(t, w.Body.String(), "Collection name was not passed")
}

func TestCollectionCount_BadRequestBody(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/count", `not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
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
}

// ---- collectionAggregate ----

func TestCollectionAggregate_MissingDatabaseParam(t *testing.T) {
	s := newTestServer(nil, "", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{"Aggregate":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
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
	assert.Contains(t, w.Body.String(), "Collection name was not passed")
}

func TestCollectionAggregate_BadRequestBody(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCollectionAggregate_NonArrayAggregateField(t *testing.T) {
	s := newTestServer(nil, "app", 100, 0)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate", `{"Aggregate":"not-an-array"}`)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Aggregate field must be an array")
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
}

func TestCollectionAggregate_LimitExceedsMax(t *testing.T) {
	s := newTestServer(nil, "app", 100, 10)
	s.createRoutes()

	w := doJSONRequest(s.router, http.MethodPost, "/api/collections/widgets/aggregate?limit=50", `{"Aggregate":[]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
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
	assert.Contains(t, w.Body.String(), "Aggregate field must be an array")
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
}
