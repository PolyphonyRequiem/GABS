package gabp

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pardeike/gabs/internal/util"
	"github.com/pardeike/gabs/internal/version"
)

// Client speaks GABP over TCP NDJSON.
type Client struct {
	conn          net.Conn
	writer        *util.LSPFrameWriter
	reader        *util.LSPFrameReader
	token         string
	agentId       string
	capabilities  Capabilities
	pendingReqs   map[string]chan *util.GABPMessage
	mu            sync.RWMutex
	log           util.Logger
	eventHandlers map[string][]EventHandler
	sequences     map[string]int
	connected     bool
	// requestTimeout is the per-request wait before giving up. Seeded from the
	// server's advertised Limits.RequestTimeout at welcome time (falling back to
	// defaultRequestTimeout). Configurable so long-running main-thread scripts
	// (asset bundle loads, world streaming) are not cut off at a fixed 30s.
	requestTimeout time.Duration
}

// defaultRequestTimeout is used when the GABP server advertises no RequestTimeout.
const defaultRequestTimeout = 30 * time.Second

// EventHandler is a function that handles events
type EventHandler func(channel string, seq int, payload interface{})

// Capabilities represents server capabilities from welcome response
type Capabilities struct {
	Methods   []string `json:"methods"`
	Events    []string `json:"events"`
	Resources []string `json:"resources"`
	Limits    *Limits  `json:"limits,omitempty"`
}

// Limits represents server limits
type Limits struct {
	MaxMessageSize        int `json:"maxMessageSize"`
	MaxConcurrentRequests int `json:"maxConcurrentRequests"`
	RequestTimeout        int `json:"requestTimeout"`
}

// SessionHelloParams represents the parameters for session/hello
type SessionHelloParams struct {
	Token         string      `json:"token"`
	BridgeVersion string      `json:"bridgeVersion"`
	Platform      string      `json:"platform"`
	LaunchId      string      `json:"launchId"`
	ClientInfo    *ClientInfo `json:"clientInfo,omitempty"`
}

// ClientInfo represents client information
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// SessionWelcomeResult represents the result of session/welcome
type SessionWelcomeResult struct {
	AgentId       string       `json:"agentId"`
	App           *AppInfo     `json:"app,omitempty"`
	Capabilities  Capabilities `json:"capabilities"`
	SchemaVersion string       `json:"schemaVersion"`
	ServerInfo    *ServerInfo  `json:"serverInfo,omitempty"`
}

// AppInfo represents application information
type AppInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ServerInfo represents server information
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Author  string `json:"author,omitempty"`
}

// NewClient creates a new GABP client
func NewClient(log util.Logger) *Client {
	// Seed the global random number generator for backoff jitter
	// Use current time with nanosecond precision to avoid identical seeds
	rand.Seed(time.Now().UnixNano())
	
	return &Client{
		pendingReqs:   make(map[string]chan *util.GABPMessage),
		eventHandlers: make(map[string][]EventHandler),
		sequences:     make(map[string]int),
		log:           log,
	}
}

func (c *Client) Connect(addr string, token string, backoffMin, backoffMax time.Duration) error {
	c.token = token

	// Connect with retry/backoff
	var conn net.Conn
	var err error

	// Implement proper exponential backoff with jitter
	// Respects backoffMin and backoffMax parameters with exponential growth
	// and randomized jitter to avoid thundering herd problems when multiple games
	// try to connect simultaneously.
	for attempts := 0; attempts < 5; attempts++ {
		conn, err = net.Dial("tcp", addr)
		if err == nil {
			break
		}
		c.log.Warnw("connection attempt failed", "attempt", attempts+1, "error", err)
		
		// Don't wait after the last attempt
		if attempts == 4 {
			break
		}
		
		// Calculate exponential backoff: backoffMin * 2^attempts
		multiplier := math.Pow(2, float64(attempts))
		backoffDelay := time.Duration(float64(backoffMin) * multiplier)
		
		// Cap at backoffMax
		if backoffDelay > backoffMax {
			backoffDelay = backoffMax
		}
		
		// Add jitter: ±25% randomization to prevent thundering herd
		jitterRange := float64(backoffDelay) * 0.25
		jitter := time.Duration(rand.Float64()*2*jitterRange - jitterRange)
		finalDelay := backoffDelay + jitter
		
		// Ensure we never go below backoffMin or above backoffMax
		if finalDelay < backoffMin {
			finalDelay = backoffMin
		}
		if finalDelay > backoffMax {
			finalDelay = backoffMax
		}
		
		c.log.Debugw("backing off before retry", "attempt", attempts+1, "delay", finalDelay, "baseDelay", backoffDelay)
		time.Sleep(finalDelay)
	}

	if err != nil {
		return fmt.Errorf("failed to connect after retries: %w", err)
	}

	c.conn = conn
	c.writer = util.NewLSPFrameWriter(conn)
	c.reader = util.NewLSPFrameReader(conn)
	c.connected = true

	// Start message handling goroutine
	go c.messageHandler()

	// Perform handshake
	return c.handshake()
}

func (c *Client) handshake() error {
	// Send session/hello
	launchId := uuid.New().String()
	params := SessionHelloParams{
		Token:         c.token,
		BridgeVersion: version.Get(), // Use actual runtime version
		Platform:      runtime.GOOS, // Detect actual platform
		LaunchId:      launchId,
		ClientInfo: &ClientInfo{
			Name:    "gabs",
			Version: version.Get(),
		},
	}

	result, err := c.sendRequest("session/hello", params)
	if err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}

	// Parse welcome response
	var welcome SessionWelcomeResult
	if err := mapToStruct(result, &welcome); err != nil {
		return fmt.Errorf("failed to parse welcome: %w", err)
	}

	c.agentId = welcome.AgentId
	c.capabilities = welcome.Capabilities

	// Seed the per-request timeout from the server's advertised limit (seconds).
	// Falls back to defaultRequestTimeout when unset/zero. This is what lets a
	// mod that knows some tools run long (asset-bundle loads, world streaming)
	// widen the budget instead of every heavy call dying at a fixed 30s.
	c.requestTimeout = defaultRequestTimeout
	if welcome.Capabilities.Limits != nil && welcome.Capabilities.Limits.RequestTimeout > 0 {
		c.requestTimeout = time.Duration(welcome.Capabilities.Limits.RequestTimeout) * time.Second
	}

	c.log.Infow("GABP handshake complete", "agentId", c.agentId, "methods", len(c.capabilities.Methods), "requestTimeout", c.requestTimeout)
	return nil
}

func (c *Client) messageHandler() {
	defer func() {
		c.connected = false
		if c.conn != nil {
			c.conn.Close()
		}
	}()

	for c.connected {
		data, err := c.reader.ReadMessage()
		if err != nil {
			c.log.Errorw("failed to read message", "error", err)
			break
		}

		var msg util.GABPMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			c.log.Errorw("failed to unmarshal message", "error", err)
			continue
		}

		c.handleMessage(&msg)
	}
}

func (c *Client) handleMessage(msg *util.GABPMessage) {
	switch msg.Type {
	case "response":
		c.handleResponse(msg)
	case "event":
		c.handleEvent(msg)
	default:
		c.log.Warnw("unknown message type", "type", msg.Type, "id", msg.ID)
	}
}

func (c *Client) handleResponse(msg *util.GABPMessage) {
	c.mu.RLock()
	ch, exists := c.pendingReqs[msg.ID]
	c.mu.RUnlock()

	if exists {
		select {
		case ch <- msg:
		case <-time.After(5 * time.Second):
			c.log.Warnw("response channel timeout", "id", msg.ID)
		}
	} else {
		c.log.Warnw("received response for unknown request", "id", msg.ID)
	}
}

func (c *Client) handleEvent(msg *util.GABPMessage) {
	c.mu.RLock()
	handlers := c.eventHandlers[msg.Channel]
	c.mu.RUnlock()

	for _, handler := range handlers {
		go handler(msg.Channel, msg.Seq, msg.Payload)
	}
}

func (c *Client) sendRequest(method string, params interface{}) (interface{}, error) {
	req := util.NewGABPRequest(method, params)

	// Register response channel
	respCh := make(chan *util.GABPMessage, 1)
	c.mu.Lock()
	c.pendingReqs[req.ID] = respCh
	c.mu.Unlock()

	// Clean up on exit
	defer func() {
		c.mu.Lock()
		delete(c.pendingReqs, req.ID)
		c.mu.Unlock()
	}()

	// Send request
	if err := c.writer.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to write request: %w", err)
	}

	// Determine the wait budget: prefer the negotiated per-client timeout,
	// fall back to the package default.
	timeout := c.requestTimeout
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	start := time.Now()

	// Wait for response
	select {
	case resp := <-respCh:
		if resp.Error != nil {
			// Surface the FULL structured error from the game side (code + message)
			// plus which method it came from — never a contentless failure.
			return nil, fmt.Errorf("GABP error on %s (id=%s): code %d: %s",
				method, req.ID, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(timeout):
		// Informative timeout: name the method, the request id, and how long we
		// actually waited, so a stuck main thread vs. a slow script vs. a wedged
		// bridge can be told apart. (This replaced an opaque "request timeout"
		// that hid real compile/runtime errors behind a generic deadline.)
		return nil, fmt.Errorf(
			"request timeout: no response to %s (id=%s) after %s "+
				"(game main thread may be blocked, or the script/tool is still running longer than the negotiated %s budget)",
			method, req.ID, time.Since(start).Round(time.Millisecond), timeout)
	}
}

// ToolParameter represents a tool parameter from Lib.GAB
type ToolParameter struct {
	Name         string      `json:"name"`
	Type         string      `json:"type"`
	Description  string      `json:"description,omitempty"`
	Required     bool        `json:"required"`
	DefaultValue interface{} `json:"defaultValue,omitempty"`
}

// ToolDescriptorRaw is the raw format from Lib.GAB
type ToolDescriptorRaw struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Parameters   []ToolParameter `json:"parameters,omitempty"`
	RequiresAuth bool            `json:"requiresAuth,omitempty"`
}

// ToolDescriptor is the normalized format for MCP
type ToolDescriptor struct {
	Name         string                 `json:"name"`
	Title        string                 `json:"title,omitempty"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  map[string]interface{} `json:"inputSchema,omitempty"`
	OutputSchema map[string]interface{} `json:"outputSchema,omitempty"`
	Tags         []string               `json:"tags,omitempty"`
}

// convertToToolDescriptor converts a raw Lib.GAB tool descriptor to MCP format
func convertToToolDescriptor(raw ToolDescriptorRaw) ToolDescriptor {
	// Build JSON Schema from parameters
	properties := make(map[string]interface{})
	required := []string{}

	for _, p := range raw.Parameters {
		prop := map[string]interface{}{
			"type": mapTypeToJSONSchema(p.Type),
		}
		if p.Description != "" {
			prop["description"] = p.Description
		}
		if p.DefaultValue != nil {
			prop["default"] = p.DefaultValue
		}
		properties[p.Name] = prop

		if p.Required {
			required = append(required, p.Name)
		}
	}

	inputSchema := map[string]interface{}{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		inputSchema["required"] = required
	}

	return ToolDescriptor{
		Name:        raw.Name,
		Description: raw.Description,
		InputSchema: inputSchema,
	}
}

// mapTypeToJSONSchema converts C# type names to JSON Schema types
func mapTypeToJSONSchema(typeName string) string {
	switch typeName {
	case "String", "string":
		return "string"
	case "Int32", "Int64", "int", "long":
		return "integer"
	case "Single", "Double", "float", "double":
		return "number"
	case "Boolean", "bool":
		return "boolean"
	default:
		return "string"
	}
}

func (c *Client) ListTools() ([]ToolDescriptor, error) {
	result, err := c.sendRequest("tools/list", map[string]interface{}{})
	if err != nil {
		return nil, err
	}

	// The response is { "tools": [...] }, so extract the tools array
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("unexpected response type: %T", result)
	}

	toolsData, exists := resultMap["tools"]
	if !exists {
		// No tools registered
		return []ToolDescriptor{}, nil
	}

	// Parse as raw format from Lib.GAB
	var rawTools []ToolDescriptorRaw
	if err := mapToStruct(toolsData, &rawTools); err != nil {
		return nil, fmt.Errorf("failed to parse tools: %w", err)
	}

	// Convert to MCP format
	tools := make([]ToolDescriptor, len(rawTools))
	for i, raw := range rawTools {
		tools[i] = convertToToolDescriptor(raw)
	}

	return tools, nil
}

func (c *Client) CallTool(name string, args map[string]any) (map[string]any, bool, error) {
	params := map[string]interface{}{
		"name":       name,
		"parameters": args,
	}

	result, err := c.sendRequest("tools/call", params)
	if err != nil {
		return nil, true, err
	}

	// Convert result to map
	resultMap, ok := result.(map[string]interface{})
	if !ok {
		return map[string]any{"value": result}, false, nil
	}

	return resultMap, false, nil
}

// SubscribeEvents subscribes to event channels
func (c *Client) SubscribeEvents(channels []string, handler EventHandler) error {
	// Register handler
	c.mu.Lock()
	for _, ch := range channels {
		c.eventHandlers[ch] = append(c.eventHandlers[ch], handler)
	}
	c.mu.Unlock()

	// Send subscription request
	params := map[string]interface{}{
		"channels": channels,
	}
	_, err := c.sendRequest("events/subscribe", params)
	return err
}

// GetCapabilities returns the server capabilities from the welcome response
func (c *Client) GetCapabilities() Capabilities {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.capabilities
}

// Close gracefully closes the GABP connection
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	if !c.connected {
		return nil
	}
	
	c.connected = false
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// mapToStruct converts a generic interface{} to a specific struct
func mapToStruct(src interface{}, dst interface{}) error {
	data, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}
