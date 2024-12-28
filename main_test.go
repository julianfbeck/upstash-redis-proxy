package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testServer *httptest.Server
	testClient *http.Client
)

func setupTest(t *testing.T) func() {
	// Set test auth token
	authToken = "test-token"

	// Create test Redis client
	redisClient = redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})

	// Create test server
	r := mux.NewRouter()
	r.Use(authMiddleware)
	r.HandleFunc("/monitor", handleMonitor).Methods(http.MethodPost)
	r.HandleFunc("/subscribe/{channel}", handleSubscribe).Methods(http.MethodPost)
	r.HandleFunc("/publish/{channel}/{message}", handlePublish).Methods(http.MethodPost)
	r.HandleFunc("/pipeline", handlePipelineEndpoint).Methods(http.MethodPost)
	r.HandleFunc("/multi-exec", handleTransactionEndpoint).Methods(http.MethodPost)
	r.PathPrefix("/").HandlerFunc(handleRedisCommand)

	testServer = httptest.NewServer(r)
	testClient = &http.Client{}

	// Clean up function
	return func() {
		testServer.Close()
		redisClient.FlushAll(context.Background())
	}
}

func makeRequest(t *testing.T, method, path string, body io.Reader, headers map[string]string) *http.Response {
	req, err := http.NewRequest(method, testServer.URL+path, body)
	require.NoError(t, err)

	// Initialize headers map if nil
	if headers == nil {
		headers = make(map[string]string)
	}

	// Add default auth header if not present and if we're not testing missing auth
	if _, exists := headers["Authorization"]; !exists && headers["SkipDefaultAuth"] != "true" {
		headers["Authorization"] = "Bearer test-token"
	}
	delete(headers, "SkipDefaultAuth") // Remove the control header before sending

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := testClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestBasicCommands(t *testing.T) {
	cleanup := setupTest(t)
	defer cleanup()

	tests := []struct {
		name           string
		method         string
		path           string
		body           io.Reader
		headers        map[string]string
		expectedStatus int
		checkResponse  func(*testing.T, *http.Response)
	}{
		{
			name:           "SET Command",
			method:         http.MethodPost,
			path:           "/set/test-key/test-value",
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, resp *http.Response) {
				var result Response
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
				assert.Equal(t, "OK", result.Result)
			},
		},
		{
			name:           "GET Command",
			method:         http.MethodGet,
			path:           "/get/test-key",
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, resp *http.Response) {
				var result Response
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
				assert.Equal(t, "test-value", result.Result)
			},
		},
		{
			name:           "DEL Command",
			method:         http.MethodPost,
			path:           "/del/test-key",
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, resp *http.Response) {
				var result Response
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
				assert.Equal(t, float64(1), result.Result)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := makeRequest(t, tt.method, tt.path, tt.body, tt.headers)
			defer resp.Body.Close()
			assert.Equal(t, tt.expectedStatus, resp.StatusCode)
			if tt.checkResponse != nil {
				tt.checkResponse(t, resp)
			}
		})
	}
}

func TestAuthentication(t *testing.T) {
	cleanup := setupTest(t)
	defer cleanup()

	tests := []struct {
		name           string
		headers        map[string]string
		expectedStatus int
	}{
		{
			name:           "Valid Token in Header",
			headers:        map[string]string{"Authorization": "Bearer test-token"},
			expectedStatus: http.StatusOK,
		},
		{
			name:           "Invalid Token",
			headers:        map[string]string{"Authorization": "Bearer wrong-token"},
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "Missing Token",
			headers:        map[string]string{"SkipDefaultAuth": "true"},
			expectedStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := makeRequest(t, http.MethodGet, "/ping", nil, tt.headers)
			defer resp.Body.Close()
			assert.Equal(t, tt.expectedStatus, resp.StatusCode)
		})
	}
}

func TestPipeline(t *testing.T) {
	cleanup := setupTest(t)
	defer cleanup()

	commands := [][]string{
		{"SET", "key1", "value1"},
		{"SET", "key2", "value2"},
		{"MGET", "key1", "key2"},
	}

	body, err := json.Marshal(commands)
	require.NoError(t, err)

	resp := makeRequest(t, http.MethodPost, "/pipeline", bytes.NewReader(body), nil)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var results []Response
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&results))
	assert.Len(t, results, 3)
	assert.Equal(t, "OK", results[0].Result)
	assert.Equal(t, "OK", results[1].Result)
}

func TestTransaction(t *testing.T) {
	cleanup := setupTest(t)
	defer cleanup()

	commands := [][]string{
		{"SET", "tx-key1", "value1"},
		{"SET", "tx-key2", "value2"},
		{"MGET", "tx-key1", "tx-key2"},
	}

	body, err := json.Marshal(commands)
	require.NoError(t, err)

	resp := makeRequest(t, http.MethodPost, "/multi-exec", bytes.NewReader(body), nil)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var results []Response
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&results))
	assert.Len(t, results, 3)
}

func TestRESP2Format(t *testing.T) {
	cleanup := setupTest(t)
	defer cleanup()

	// First set a value
	makeRequest(t, http.MethodPost, "/set/resp2-key/resp2-value", nil, nil)

	// Then get it with RESP2 format
	headers := map[string]string{
		"Upstash-Response-Format": "resp2",
	}
	resp := makeRequest(t, http.MethodGet, "/get/resp2-key", nil, headers)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, "$11\r\nresp2-value\r\n", string(body))
}

func TestBase64Encoding(t *testing.T) {
	cleanup := setupTest(t)
	defer cleanup()

	// First set a value
	makeRequest(t, http.MethodPost, "/set/base64-key/test-value", nil, nil)

	// Then get it with base64 encoding
	headers := map[string]string{
		"Upstash-Encoding": "base64",
	}
	resp := makeRequest(t, http.MethodGet, "/get/base64-key", nil, headers)
	defer resp.Body.Close()

	var result Response
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))

	decoded, err := base64.StdEncoding.DecodeString(result.Result.(string))
	require.NoError(t, err)
	assert.Equal(t, "test-value", string(decoded))
}

func TestPubSub(t *testing.T) {
	cleanup := setupTest(t)
	defer cleanup()

	// Clean up any existing subscriptions
	redisClient.FlushAll(context.Background())

	// Channel to signal when we've received the message
	messageReceived := make(chan struct{})
	subscribeDone := make(chan struct{})

	// Start a goroutine to subscribe
	go func() {
		defer close(subscribeDone)

		resp := makeRequest(t, http.MethodPost, "/subscribe/test-channel", nil, map[string]string{
			"Accept": "text/event-stream",
		})
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

		reader := bufio.NewReader(resp.Body)

		// Read the subscription confirmation
		// Expect: event: subscribe\ndata: {"channel":"test-channel","count":1}\n\n
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, "event: subscribe\n", line)

		line, err = reader.ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, "data: {\"channel\":\"test-channel\",\"count\":1}\n", line)

		line, err = reader.ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, "\n", line)

		// Read the message
		// Expect: event: message\ndata: {"channel":"test-channel","message":"hello"}\n\n
		line, err = reader.ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, "event: message\n", line)

		line, err = reader.ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, "data: {\"channel\":\"test-channel\",\"message\":\"hello\"}\n", line)

		line, err = reader.ReadString('\n')
		require.NoError(t, err)
		assert.Equal(t, "\n", line)

		close(messageReceived)
	}()

	// Wait a bit for subscription to be ready
	time.Sleep(100 * time.Millisecond)

	// Publish a message
	resp := makeRequest(t, http.MethodPost, "/publish/test-channel/hello", nil, nil)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var result Response
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Equal(t, float64(1), result.Result) // Expect 1 subscriber

	// Wait for the message to be received or timeout
	select {
	case <-messageReceived:
		// Success
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for message")
	}

	// Signal subscriber to stop
	resp.Body.Close()
	<-subscribeDone
}

func TestMonitor(t *testing.T) {
	cleanup := setupTest(t)
	defer cleanup()

	// Start monitor in a goroutine
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)

		resp := makeRequest(t, http.MethodPost, "/monitor", nil, map[string]string{
			"Accept": "text/event-stream",
		})
		defer resp.Body.Close()

		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

		// Read the first message (OK confirmation)
		reader := bufio.NewReader(resp.Body)
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		assert.Contains(t, line, "OK")
	}()

	// Wait a bit for monitor to be ready
	time.Sleep(100 * time.Millisecond)

	// Execute a command that should be monitored
	makeRequest(t, http.MethodPost, "/set/monitor-key/monitor-value", nil, nil)

	// Wait for monitor to process
	<-monitorDone
}

func TestQueryParameters(t *testing.T) {
	cleanup := setupTest(t)
	defer cleanup()

	// Test SET with EX parameter
	resp := makeRequest(t, http.MethodPost, "/set/expiry-key/expiry-value?EX=1", nil, nil)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var result Response
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Equal(t, "OK", result.Result)

	// Wait for key to expire
	time.Sleep(2 * time.Second)

	// Verify key has expired
	resp = makeRequest(t, http.MethodGet, "/get/expiry-key", nil, nil)
	defer resp.Body.Close()

	var getResult Response
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&getResult))
	assert.Nil(t, getResult.Result)
}
