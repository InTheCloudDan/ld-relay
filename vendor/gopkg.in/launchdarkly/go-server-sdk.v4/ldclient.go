package ldclient

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"time"
)

// Version is the client version.
const Version = "4.7.4"

// LDClient is the LaunchDarkly client. Client instances are thread-safe.
// Applications should instantiate a single instance for the lifetime
// of their application.
type LDClient struct {
	sdkKey          string
	config          Config
	eventProcessor  EventProcessor
	updateProcessor UpdateProcessor
	store           FeatureStore
}

// Logger is a generic logger interface.
type Logger interface {
	Println(...interface{})
	Printf(string, ...interface{})
}

// Config exposes advanced configuration options for the LaunchDarkly client.
type Config struct {
	// The base URI of the main LaunchDarkly service. This should not normally be changed except for testing.
	BaseUri string
	// The base URI of the LaunchDarkly streaming service. This should not normally be changed except for testing.
	StreamUri string
	// The base URI of the LaunchDarkly service that accepts analytics events. This should not normally be
	// changed except for testing.
	EventsUri string
	// The full URI for posting analytics events. This is different from EventsUri in that the client will not
	// add the default URI path to it. It should not normally be changed except for testing, and if set, it
	// causes EventsUri to be ignored.
	EventsEndpointUri string
	// The capacity of the events buffer. The client buffers up to this many events in memory before flushing.
	// If the capacity is exceeded before the buffer is flushed, events will be discarded.
	Capacity int
	// The time between flushes of the event buffer. Decreasing the flush interval means that the event buffer
	// is less likely to reach capacity.
	FlushInterval time.Duration
	// Enables event sampling if non-zero. When set to the default of zero, all events are sent to Launchdarkly.
	// If greater than zero, there is a 1 in SamplingInterval chance that events will be sent (for example, a
	// value of 20 means on average 5% of events will be sent).
	SamplingInterval int32
	// The polling interval (when streaming is disabled). Values less than the default of MinimumPollInterval
	// will be set to the default.
	PollInterval time.Duration
	// An object
	Logger Logger
	// The connection timeout to use when making polling requests to LaunchDarkly.
	Timeout time.Duration
	// Sets the implementation of FeatureStore for holding feature flags and related data received from
	// LaunchDarkly. See NewInMemoryFeatureStore (the default) and the redis, ldconsul, and lddynamodb packages.
	FeatureStore FeatureStore
	// Sets whether streaming mode should be enabled. By default, streaming is enabled. It should only be
	// disabled on the advice of LaunchDarkly support.
	Stream bool
	// Sets whether this client should use the LaunchDarkly relay in daemon mode. In this mode, the client does
	// not subscribe to the streaming or polling API, but reads data only from the feature store. See:
	// https://docs.launchdarkly.com/docs/the-relay-proxy
	UseLdd bool
	// Sets whether to send analytics events back to LaunchDarkly. By default, the client will send events. This
	// differs from Offline in that it only affects sending events, not streaming or polling for events from the
	// server.
	SendEvents bool
	// Sets whether this client is offline. An offline client will not make any network connections to LaunchDarkly,
	// and will return default values for all feature flags.
	Offline bool
	// Sets whether or not all user attributes (other than the key) should be hidden from LaunchDarkly. If this
	// is true, all user attribute values will be private, not just the attributes specified in PrivateAttributeNames.
	AllAttributesPrivate bool
	// Set to true if you need to see the full user details in every analytics event.
	InlineUsersInEvents bool
	// Marks a set of user attribute names private. Any users sent to LaunchDarkly with this configuration
	// active will have attributes with these names removed.
	PrivateAttributeNames []string
	// Deprecated. Please use UpdateProcessorFactory.
	UpdateProcessor UpdateProcessor
	// Factory to create an object that is responsible for receiving feature flag updates from LaunchDarkly.
	// If nil, a default implementation will be used depending on the rest of the configuration
	// (streaming, polling, etc.); a custom implementation can be substituted for testing.
	UpdateProcessorFactory UpdateProcessorFactory
	// An object that is responsible for recording or sending analytics events. If nil, a
	// default implementation will be used; a custom implementation can be substituted for testing.
	EventProcessor EventProcessor
	// The number of user keys that the event processor can remember at any one time, so that
	// duplicate user details will not be sent in analytics events.
	UserKeysCapacity int
	// The interval at which the event processor will reset its set of known user keys.
	UserKeysFlushInterval time.Duration
	UserAgent             string
}

// MinimumPollInterval describes the minimum value for Config.PollInterval. If you specify a smaller interval,
// the minimum will be used instead.
const MinimumPollInterval = 30 * time.Second

// UpdateProcessor describes the interface for an object that receives feature flag data.
type UpdateProcessor interface {
	Initialized() bool
	Close() error
	Start(closeWhenReady chan<- struct{})
}

// UpdateProcessorFactory is a function that creates an UpdateProcessor.
type UpdateProcessorFactory func(sdkKey string, config Config) (UpdateProcessor, error)

type nullUpdateProcessor struct{}

func (n nullUpdateProcessor) Initialized() bool {
	return true
}

func (n nullUpdateProcessor) Close() error {
	return nil
}

func (n nullUpdateProcessor) Start(closeWhenReady chan<- struct{}) {
	close(closeWhenReady)
}

// DefaultConfig provides the default configuration options for the LaunchDarkly client.
// The easiest way to create a custom configuration is to start with the
// default config, and set the custom options from there. For example:
//   var config = DefaultConfig
//   config.Capacity = 2000
var DefaultConfig = Config{
	BaseUri:               "https://app.launchdarkly.com",
	StreamUri:             "https://stream.launchdarkly.com",
	EventsUri:             "https://events.launchdarkly.com",
	Capacity:              10000,
	FlushInterval:         5 * time.Second,
	PollInterval:          MinimumPollInterval,
	Logger:                log.New(os.Stderr, "[LaunchDarkly]", log.LstdFlags),
	Timeout:               3000 * time.Millisecond,
	Stream:                true,
	FeatureStore:          nil,
	UseLdd:                false,
	SendEvents:            true,
	Offline:               false,
	UserKeysCapacity:      1000,
	UserKeysFlushInterval: 5 * time.Minute,
	UserAgent:             "",
}

// Initialization errors
var (
	ErrInitializationTimeout = errors.New("timeout encountered waiting for LaunchDarkly client initialization")
	ErrInitializationFailed  = errors.New("LaunchDarkly client initialization failed")
	ErrClientNotInitialized  = errors.New("feature flag evaluation called before LaunchDarkly client initialization completed")
)

// MakeClient creates a new client instance that connects to LaunchDarkly with the default configuration. In most
// cases, you should use this method to instantiate your client. The optional duration parameter allows callers to
// block until the client has connected to LaunchDarkly and is properly initialized.
func MakeClient(sdkKey string, waitFor time.Duration) (*LDClient, error) {
	return MakeCustomClient(sdkKey, DefaultConfig, waitFor)
}

// MakeCustomClient creates a new client instance that connects to LaunchDarkly with a custom configuration. The optional duration parameter allows callers to
// block until the client has connected to LaunchDarkly and is properly initialized.
func MakeCustomClient(sdkKey string, config Config, waitFor time.Duration) (*LDClient, error) {
	closeWhenReady := make(chan struct{})

	config.BaseUri = strings.TrimRight(config.BaseUri, "/")
	config.EventsUri = strings.TrimRight(config.EventsUri, "/")
	if config.PollInterval < MinimumPollInterval {
		config.PollInterval = MinimumPollInterval
	}
	config.UserAgent = strings.TrimSpace("GoClient/" + Version + " " + config.UserAgent)

	if config.FeatureStore == nil {
		config.FeatureStore = NewInMemoryFeatureStore(config.Logger)
	}

	client := LDClient{
		sdkKey: sdkKey,
		config: config,
		store:  config.FeatureStore,
	}

	if config.EventProcessor != nil {
		client.eventProcessor = config.EventProcessor
	} else if config.SendEvents && !config.Offline {
		client.eventProcessor = NewDefaultEventProcessor(sdkKey, config, nil)
	} else {
		client.eventProcessor = newNullEventProcessor()
	}

	if config.UpdateProcessor != nil {
		client.updateProcessor = config.UpdateProcessor
	} else {
		factory := config.UpdateProcessorFactory
		if factory == nil {
			factory = createDefaultUpdateProcessor
		}
		var err error
		client.updateProcessor, err = factory(sdkKey, config)
		if err != nil {
			return nil, err
		}
	}
	client.updateProcessor.Start(closeWhenReady)
	timeout := time.After(waitFor)
	for {
		select {
		case <-closeWhenReady:
			if !client.updateProcessor.Initialized() {
				return &client, ErrInitializationFailed
			}

			config.Logger.Println("Successfully initialized LaunchDarkly client!")
			return &client, nil
		case <-timeout:
			if waitFor > 0 {
				config.Logger.Println("Timeout exceeded when initializing LaunchDarkly client.")
				return &client, ErrInitializationTimeout
			}

			go func() { <-closeWhenReady }() // Don't block the UpdateProcessor when not waiting
			return &client, nil
		}
	}
}

func createDefaultUpdateProcessor(sdkKey string, config Config) (UpdateProcessor, error) {
	if config.Offline {
		config.Logger.Println("Started LaunchDarkly in offline mode")
		return nullUpdateProcessor{}, nil
	}
	if config.UseLdd {
		config.Logger.Println("Started LaunchDarkly in LDD mode")
		return nullUpdateProcessor{}, nil
	}
	requestor := newRequestor(sdkKey, config)
	if config.Stream {
		return newStreamProcessor(sdkKey, config, requestor), nil
	}
	config.Logger.Println("You should only disable the streaming API if instructed to do so by LaunchDarkly support")
	return newPollingProcessor(config, requestor), nil
}

// Identify reports details about a a user.
func (client *LDClient) Identify(user User) error {
	if client.IsOffline() {
		return nil
	}
	if user.Key == nil || *user.Key == "" {
		client.config.Logger.Printf("WARN: Identify called with empty/nil user key!")
		return nil // Don't return an error value because we didn't in the past and it might confuse users
	}
	evt := NewIdentifyEvent(user)
	client.eventProcessor.SendEvent(evt)
	return nil
}

// Track reports that a user has performed an event. Custom data can be attached to the
// event, and is serialized to JSON using the encoding/json package (http://golang.org/pkg/encoding/json/).
func (client *LDClient) Track(key string, user User, data interface{}) error {
	if client.IsOffline() {
		return nil
	}
	if user.Key == nil || *user.Key == "" {
		client.config.Logger.Printf("WARN: Track called with empty/nil user key!")
		return nil // Don't return an error value because we didn't in the past and it might confuse users
	}
	evt := NewCustomEvent(key, user, data)
	client.eventProcessor.SendEvent(evt)
	return nil
}

// IsOffline returns whether the LaunchDarkly client is in offline mode.
func (client *LDClient) IsOffline() bool {
	return client.config.Offline
}

// SecureModeHash generates the secure mode hash value for a user
// See https://github.com/launchdarkly/js-client#secure-mode
func (client *LDClient) SecureModeHash(user User) string {
	if user.Key == nil {
		return ""
	}
	key := []byte(client.sdkKey)
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(*user.Key))
	return hex.EncodeToString(h.Sum(nil))
}

// Initialized returns whether the LaunchDarkly client is initialized.
func (client *LDClient) Initialized() bool {
	return client.IsOffline() || client.config.UseLdd || client.updateProcessor.Initialized()
}

// Close shuts down the LaunchDarkly client. After calling this, the LaunchDarkly client
// should no longer be used.
func (client *LDClient) Close() error {
	client.config.Logger.Println("Closing LaunchDarkly Client")
	if client.IsOffline() {
		return nil
	}
	_ = client.eventProcessor.Close()
	if !client.config.UseLdd {
		_ = client.updateProcessor.Close()
	}
	return nil
}

// Flush immediately flushes queued events.
func (client *LDClient) Flush() {
	client.eventProcessor.Flush()
}

// AllFlags returns a map from feature flag keys to values for
// a given user. If the result of the flag's evaluation would
// result in the default value, `nil` will be returned. This method
// does not send analytics events back to LaunchDarkly
//
// Deprecated: Use AllFlagsState instead. Current versions of the client-side SDK
// will not generate analytics events correctly if you pass the result of AllFlags.
func (client *LDClient) AllFlags(user User) map[string]interface{} {
	state := client.AllFlagsState(user)
	return state.ToValuesMap()
}

// AllFlagsState returns an object that encapsulates the state of all feature flags for a
// given user, including the flag values and also metadata that can be used on the front end.
// You may pass any combination of ClientSideOnly, WithReasons, and DetailsOnlyForTrackedFlags
// as optional parameters to control what data is included.
//
// The most common use case for this method is to bootstrap a set of client-side feature flags
// from a back-end service.
func (client *LDClient) AllFlagsState(user User, options ...FlagsStateOption) FeatureFlagsState {
	valid := true
	if client.IsOffline() {
		client.config.Logger.Println("WARN: Called AllFlagsState in offline mode. Returning empty state")
		valid = false
	} else if user.Key == nil {
		client.config.Logger.Println("WARN: Called AllFlagsState with nil user key. Returning empty state")
		valid = false
	} else if !client.Initialized() {
		if client.store.Initialized() {
			client.config.Logger.Println("WARN: Called AllFlagsState before client initialization; using last known values from feature store")
		} else {
			client.config.Logger.Println("WARN: Called AllFlagsState before client initialization. Feature store not available; returning empty state")
			valid = false
		}
	}

	if !valid {
		return FeatureFlagsState{valid: false}
	}

	items, err := client.store.All(Features)
	if err != nil {
		client.config.Logger.Println("WARN: Unable to fetch flags from feature store. Returning empty state. Error: " + err.Error())
		return FeatureFlagsState{valid: false}
	}

	state := newFeatureFlagsState()
	clientSideOnly := hasFlagsStateOption(options, ClientSideOnly)
	withReasons := hasFlagsStateOption(options, WithReasons)
	detailsOnlyIfTracked := hasFlagsStateOption(options, DetailsOnlyForTrackedFlags)
	for _, item := range items {
		if flag, ok := item.(*FeatureFlag); ok {
			if clientSideOnly && !flag.ClientSide {
				continue
			}
			result, _ := flag.EvaluateDetail(user, client.store, false)
			var reason EvaluationReason
			if withReasons {
				reason = result.Reason
			}
			state.addFlag(flag, result.Value, result.VariationIndex, reason, detailsOnlyIfTracked)
		}
	}

	return state
}

// BoolVariation returns the value of a boolean feature flag for a given user. Returns defaultVal if
// there is an error, if the flag doesn't exist, the client hasn't completed initialization,
// or the feature is turned off and has no off variation.
func (client *LDClient) BoolVariation(key string, user User, defaultVal bool) (bool, error) {
	detail, err := client.variationWithType(key, user, defaultVal, reflect.TypeOf(true), false)
	result, _ := detail.Value.(bool)
	return result, err
}

// BoolVariationDetail is the same as BoolVariation, but also returns further information about how
// the value was calculated. The "reason" data will also be included in analytics events.
func (client *LDClient) BoolVariationDetail(key string, user User, defaultVal bool) (bool, EvaluationDetail, error) {
	detail, err := client.variationWithType(key, user, defaultVal, reflect.TypeOf(true), true)
	result, _ := detail.Value.(bool)
	return result, detail, err
}

// IntVariation returns the value of a feature flag (whose variations are integers) for the given user.
// Returns defaultVal if there is an error, if the flag doesn't exist, or the feature is turned off and
// has no off variation.
func (client *LDClient) IntVariation(key string, user User, defaultVal int) (int, error) {
	detail, err := client.variationWithType(key, user, float64(defaultVal), reflect.TypeOf(float64(0)), false)
	result, _ := detail.Value.(float64)
	return int(result), err
}

// IntVariationDetail is the same as IntVariation, but also returns further information about how
// the value was calculated. The "reason" data will also be included in analytics events.
func (client *LDClient) IntVariationDetail(key string, user User, defaultVal int) (int, EvaluationDetail, error) {
	detail, err := client.variationWithType(key, user, float64(defaultVal), reflect.TypeOf(float64(0)), true)
	result, _ := detail.Value.(float64)
	return int(result), detail, err
}

// Float64Variation returns the value of a feature flag (whose variations are floats) for the given user.
// Returns defaultVal if there is an error, if the flag doesn't exist, or the feature is turned off and
// has no off variation.
func (client *LDClient) Float64Variation(key string, user User, defaultVal float64) (float64, error) {
	detail, err := client.variationWithType(key, user, defaultVal, reflect.TypeOf(float64(0)), false)
	result, _ := detail.Value.(float64)
	return result, err
}

// Float64VariationDetail is the same as Float64Variation, but also returns further information about how
// the value was calculated. The "reason" data will also be included in analytics events.
func (client *LDClient) Float64VariationDetail(key string, user User, defaultVal float64) (float64, EvaluationDetail, error) {
	detail, err := client.variationWithType(key, user, defaultVal, reflect.TypeOf(float64(0)), true)
	result, _ := detail.Value.(float64)
	return result, detail, err
}

// StringVariation returns the value of a feature flag (whose variations are strings) for the given user.
// Returns defaultVal if there is an error, if the flag doesn't exist, or the feature is turned off and has
// no off variation.
func (client *LDClient) StringVariation(key string, user User, defaultVal string) (string, error) {
	detail, err := client.variationWithType(key, user, defaultVal, reflect.TypeOf(string("string")), false)
	result, _ := detail.Value.(string)
	return result, err
}

// StringVariationDetail is the same as StringVariation, but also returns further information about how
// the value was calculated. The "reason" data will also be included in analytics events.
func (client *LDClient) StringVariationDetail(key string, user User, defaultVal string) (string, EvaluationDetail, error) {
	detail, err := client.variationWithType(key, user, defaultVal, reflect.TypeOf(string("string")), true)
	result, _ := detail.Value.(string)
	return result, detail, err
}

// JsonVariation returns the value of a feature flag (whose variations are JSON) for the given user.
// Returns defaultVal if there is an error, if the flag doesn't exist, or the feature is turned off.
func (client *LDClient) JsonVariation(key string, user User, defaultVal json.RawMessage) (json.RawMessage, error) {
	detail, err := client.variation(key, user, defaultVal, false)
	if err != nil {
		return defaultVal, err
	}
	valueJSONRawMessage, err := ToJsonRawMessage(detail.Value)
	if err != nil {
		return defaultVal, err
	}
	return valueJSONRawMessage, nil
}

// JsonVariationDetail is the same as JsonVariation, but also returns further information about how
// the value was calculated. The "reason" data will also be included in analytics events.
func (client *LDClient) JsonVariationDetail(key string, user User, defaultVal json.RawMessage) (json.RawMessage, EvaluationDetail, error) {
	detail, err := client.variation(key, user, defaultVal, true)
	if err != nil {
		return defaultVal, detail, err
	}
	valueJSONRawMessage, err := ToJsonRawMessage(detail.Value)
	if err != nil {
		detail.Value = defaultVal
		return defaultVal, detail, err
	}
	return valueJSONRawMessage, detail, nil
}

// Generic method for evaluating a feature flag for a given user. The type of the returned interface{}
// will always be expectedType or the actual defaultValue will be returned.
func (client *LDClient) variationWithType(key string, user User, defaultVal interface{}, expectedType reflect.Type, sendReasonsInEvents bool) (EvaluationDetail, error) {
	result, err := client.variation(key, user, defaultVal, sendReasonsInEvents)
	if err != nil && result.Value != nil {
		valueType := reflect.TypeOf(result.Value)
		if expectedType != valueType {
			result.Value = defaultVal
			result.VariationIndex = nil
			result.Reason = newEvalReasonError(EvalErrorWrongType)
		}
	}
	return result, err
}

// Generic method for evaluating a feature flag for a given user.
func (client *LDClient) variation(key string, user User, defaultVal interface{}, sendReasonsInEvents bool) (EvaluationDetail, error) {
	if client.IsOffline() {
		return EvaluationDetail{Value: defaultVal, Reason: newEvalReasonError(EvalErrorClientNotReady)}, nil
	}
	result, flag, err := client.evaluateInternal(key, user, defaultVal, sendReasonsInEvents)
	if err != nil {
		result.Value = defaultVal
		result.VariationIndex = nil
	}

	evt := NewFeatureRequestEvent(key, flag, user, result.VariationIndex, result.Value, defaultVal, nil)
	if sendReasonsInEvents {
		evt.Reason.Reason = result.Reason
	}
	client.eventProcessor.SendEvent(evt)

	return result, err
}

// Evaluate returns the value of a feature for a specified user
func (client *LDClient) Evaluate(key string, user User, defaultVal interface{}) (interface{}, *int, error) {
	result, _, err := client.evaluateInternal(key, user, defaultVal, false)
	return result.Value, result.VariationIndex, err
}

// Performs all the steps of evaluation except for sending the feature request event (the main one;
// events for prerequisites will be sent).
func (client *LDClient) evaluateInternal(key string, user User, defaultVal interface{}, sendReasonsInEvents bool) (EvaluationDetail, *FeatureFlag, error) {
	if user.Key != nil && *user.Key == "" {
		client.config.Logger.Printf("WARN: User.Key is blank when evaluating flag: %s. Flag evaluation will proceed, but the user will not be stored in LaunchDarkly.", key)
	}

	var feature *FeatureFlag
	var storeErr error
	var ok bool

	if !client.Initialized() {
		if client.store.Initialized() {
			client.config.Logger.Printf("WARN: Feature flag evaluation called before LaunchDarkly client initialization completed; using last known values from feature store")
		} else {
			detail := EvaluationDetail{Value: defaultVal, Reason: newEvalReasonError(EvalErrorClientNotReady)}
			return detail, nil, ErrClientNotInitialized
		}
	}

	data, storeErr := client.store.Get(Features, key)

	if storeErr != nil {
		client.config.Logger.Printf("Encountered error fetching feature from store: %+v", storeErr)
		detail := EvaluationDetail{Value: defaultVal, Reason: newEvalReasonError(EvalErrorException)}
		return detail, nil, storeErr
	}

	if data != nil {
		feature, ok = data.(*FeatureFlag)
		if !ok {
			detail := EvaluationDetail{Value: defaultVal, Reason: newEvalReasonError(EvalErrorException)}
			return detail, nil, fmt.Errorf("unexpected data type (%T) found in store for feature key: %s. Returning default value", data, key)
		}
	} else {
		detail := EvaluationDetail{Value: defaultVal, Reason: newEvalReasonError(EvalErrorFlagNotFound)}
		return detail, nil, fmt.Errorf("unknown feature key: %s Verify that this feature key exists. Returning default value", key)
	}

	if user.Key == nil {
		detail := EvaluationDetail{Value: defaultVal, Reason: newEvalReasonError(EvalErrorUserNotSpecified)}
		return detail, feature, fmt.Errorf("user.Key cannot be nil for user: %+v when evaluating flag: %s", user, key)
	}

	detail, prereqEvents := feature.EvaluateDetail(user, client.store, sendReasonsInEvents)
	if detail.IsDefaultValue() {
		detail.Value = defaultVal
	}
	for _, event := range prereqEvents {
		client.eventProcessor.SendEvent(event)
	}
	return detail, feature, nil
}
