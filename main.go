package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-redis/redis/v8"
	"github.com/gorilla/mux"
)

var (
	redisClient *redis.Client
	authToken   = os.Getenv("AUTH_TOKEN")
)

type Response struct {
	Result interface{} `json:"result,omitempty"`
	Error  string      `json:"error,omitempty"`
}

func main() {
	// Initialize Redis client
	redisClient = redis.NewClient(&redis.Options{
		Addr:     "localhost:6379",
		Password: "", // no password set
		DB:       0,  // use default DB
	})

	// Set default auth token if not provided
	if authToken == "" {
		authToken = "default-token"
		log.Printf("Warning: Using default authentication token: %s", authToken)
	}

	r := mux.NewRouter()
	r.Use(authMiddleware)

	// Special command handlers
	r.HandleFunc("/monitor", handleMonitor).Methods(http.MethodPost)
	r.HandleFunc("/subscribe/{channel}", handleSubscribe).Methods(http.MethodPost)
	r.HandleFunc("/publish/{channel}/{message}", handlePublish).Methods(http.MethodPost)
	r.HandleFunc("/pipeline", handlePipelineEndpoint).Methods(http.MethodPost)
	r.HandleFunc("/multi-exec", handleTransactionEndpoint).Methods(http.MethodPost)

	// Handle all other Redis commands
	r.PathPrefix("/").HandlerFunc(handleRedisCommand)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Starting server on port %s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}

func handleMonitor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Start MONITOR command
	cmd := redisClient.Do(ctx, "MONITOR")
	if cmd.Err() != nil {
		http.Error(w, cmd.Err().Error(), http.StatusInternalServerError)
		return
	}

	// Create a new pubsub client for MONITOR
	pubsub := redisClient.Subscribe(ctx)
	defer pubsub.Close()

	// Send initial OK
	fmt.Fprintf(w, "data: \"OK\"\n\n")
	w.(http.Flusher).Flush()

	// Listen for messages
	ch := pubsub.Channel()
	done := ctx.Done()

	for {
		select {
		case <-done:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg.Payload)
			w.(http.Flusher).Flush()
		}
	}
}

func handleSubscribe(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	channel := vars["channel"]
	ctx := r.Context()

	// Set headers for SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Subscribe to channel
	pubsub := redisClient.Subscribe(ctx, channel)
	defer pubsub.Close()

	// Send subscription confirmation
	fmt.Fprintf(w, "event: subscribe\ndata: {\"channel\":\"%s\",\"count\":1}\n\n", channel)
	w.(http.Flusher).Flush()

	// Listen for messages
	ch := pubsub.Channel()
	done := ctx.Done()

	for {
		select {
		case <-done:
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: message\ndata: {\"channel\":\"%s\",\"message\":\"%s\"}\n\n", msg.Channel, msg.Payload)
			w.(http.Flusher).Flush()
		}
	}
}

func handlePublish(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	channel := vars["channel"]
	message := vars["message"]

	// Get number of subscribers before publishing
	numSub := redisClient.PubSubNumSub(r.Context(), channel).Val()[channel]

	// Publish the message (ignore result since we're returning numSub)
	_, err := redisClient.Publish(r.Context(), channel, message).Result()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(Response{Error: err.Error()})
		return
	}

	// Return the number of subscribers that received the message
	json.NewEncoder(w).Encode(Response{Result: numSub})
}

func handlePipelineEndpoint(w http.ResponseWriter, r *http.Request) {
	result, err := handlePipeline(r.Context(), r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(Response{Error: err.Error()})
		return
	}

	// Convert []interface{} to array of results
	responses := result.([]interface{})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responses)
}

func handleTransactionEndpoint(w http.ResponseWriter, r *http.Request) {
	result, err := handleTransaction(r.Context(), r)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(Response{Error: err.Error()})
		return
	}

	// Convert []interface{} to array of results
	responses := result.([]interface{})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responses)
}

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")

		// Check URL parameter if header is not present
		if token == "" {
			token = r.URL.Query().Get("_token")
		}

		if token == "" || token != authToken {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(Response{Error: "unauthorized"})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func handleRedisCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodPut {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(Response{Error: "method not allowed"})
		return
	}

	// Parse command and arguments from URL path
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(Response{Error: "invalid command"})
		return
	}

	cmd := strings.ToUpper(parts[0])
	args := parts[1:]

	// Handle POST body if present
	if r.Method == http.MethodPost {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(Response{Error: fmt.Sprintf("error reading body: %v", err)})
			return
		}
		if len(body) > 0 {
			args = append(args, string(body))
		}
	}

	// Add query parameters as additional arguments
	for key, values := range r.URL.Query() {
		if key != "_token" { // Skip auth token
			args = append(args, key)
			args = append(args, values...)
		}
	}

	// Execute Redis command
	result, err := executeRedisCommand(r.Context(), cmd, args...)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(Response{Error: err.Error()})
		return
	}

	formatAndSendResponse(w, r, result)
}

func formatAndSendResponse(w http.ResponseWriter, r *http.Request, result interface{}) {
	responseFormat := r.Header.Get("Upstash-Response-Format")
	if responseFormat == "resp2" {
		// Format as RESP2
		w.Header().Set("Content-Type", "application/octet-stream")
		resp := formatRESP2(result)
		w.Write([]byte(resp))
		return
	}

	// Format as JSON
	w.Header().Set("Content-Type", "application/json")

	// Handle base64 encoding if requested
	if r.Header.Get("Upstash-Encoding") == "base64" {
		result = encodeBase64(result)
	}

	// For pipeline results, return array of results
	if results, ok := result.([]interface{}); ok {
		wrappedResults := make([]map[string]interface{}, len(results))
		for i, res := range results {
			if err, ok := res.(error); ok {
				wrappedResults[i] = map[string]interface{}{
					"error": err.Error(),
				}
			} else {
				wrappedResults[i] = map[string]interface{}{
					"result": res,
				}
			}
		}
		json.NewEncoder(w).Encode(wrappedResults)
		return
	}

	// For single results, wrap in a result field
	if err, ok := result.(error); ok {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": err.Error(),
		})
	} else {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"result": result,
		})
	}
}

func formatRESP2(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return "$-1\r\n"
	case string:
		return fmt.Sprintf("$%d\r\n%s\r\n", len(val), val)
	case int64:
		return fmt.Sprintf(":%d\r\n", val)
	case []interface{}:
		resp := fmt.Sprintf("*%d\r\n", len(val))
		for _, item := range val {
			resp += formatRESP2(item)
		}
		return resp
	case error:
		return fmt.Sprintf("-ERR %s\r\n", val.Error())
	default:
		return fmt.Sprintf("$%d\r\n%v\r\n", len(fmt.Sprintf("%v", val)), val)
	}
}

func executeRedisCommand(ctx context.Context, cmd string, args ...string) (interface{}, error) {
	redisCmd := redisClient.Do(ctx, append([]interface{}{cmd}, stringsToInterfaces(args)...)...)
	return redisCmd.Result()
}

func handlePipeline(ctx context.Context, r *http.Request) (interface{}, error) {
	var cmds [][]interface{}
	if err := json.NewDecoder(r.Body).Decode(&cmds); err != nil {
		return nil, fmt.Errorf("invalid pipeline request: %v", err)
	}

	pipe := redisClient.Pipeline()
	for _, cmd := range cmds {
		if len(cmd) == 0 {
			continue
		}
		pipe.Do(ctx, cmd...)
	}

	results, err := pipe.Exec(ctx)
	if err != nil {
		return nil, err
	}

	// Return raw results instead of wrapping in Response
	rawResults := make([]interface{}, len(results))
	for i, result := range results {
		if result.Err() != nil {
			rawResults[i] = map[string]interface{}{
				"error": result.Err().Error(),
			}
		} else {
			val, _ := result.(*redis.Cmd).Result()
			// Handle base64 encoding if requested
			if r.Header.Get("Upstash-Encoding") == "base64" {
				val = encodeBase64(val)
			}
			rawResults[i] = map[string]interface{}{
				"result": val,
			}
		}
	}

	return rawResults, nil
}

func handleTransaction(ctx context.Context, r *http.Request) (interface{}, error) {
	var cmds [][]interface{}
	if err := json.NewDecoder(r.Body).Decode(&cmds); err != nil {
		return nil, fmt.Errorf("invalid transaction request: %v", err)
	}

	tx := redisClient.TxPipeline()
	for _, cmd := range cmds {
		if len(cmd) == 0 {
			continue
		}
		tx.Do(ctx, cmd...)
	}

	results, err := tx.Exec(ctx)
	if err != nil {
		return nil, err
	}

	// Return raw results instead of wrapping in Response
	rawResults := make([]interface{}, len(results))
	for i, result := range results {
		if result.Err() != nil {
			rawResults[i] = map[string]interface{}{
				"error": result.Err().Error(),
			}
		} else {
			val, _ := result.(*redis.Cmd).Result()
			// Handle base64 encoding if requested
			if r.Header.Get("Upstash-Encoding") == "base64" {
				val = encodeBase64(val)
			}
			rawResults[i] = map[string]interface{}{
				"result": val,
			}
		}
	}

	return rawResults, nil
}

func stringsToInterfaces(strs []string) []interface{} {
	interfaces := make([]interface{}, len(strs))
	for i, s := range strs {
		// Try to convert to int64 first
		if num, err := strconv.ParseInt(s, 10, 64); err == nil {
			interfaces[i] = num
			continue
		}
		// Try to convert to float64
		if num, err := strconv.ParseFloat(s, 64); err == nil {
			interfaces[i] = num
			continue
		}
		// If not a number, keep as string
		interfaces[i] = s
	}
	return interfaces
}

func encodeBase64(v interface{}) interface{} {
	switch val := v.(type) {
	case string:
		if val == "OK" {
			return val
		}
		return base64.StdEncoding.EncodeToString([]byte(val))
	case []byte:
		return base64.StdEncoding.EncodeToString(val)
	case []interface{}:
		result := make([]interface{}, len(val))
		for i, item := range val {
			result[i] = encodeBase64(item)
		}
		return result
	default:
		return val
	}
}
