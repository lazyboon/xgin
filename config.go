package xgin

import "fmt"

const (
	DefaultValidatorMaxDepth       = 20      // Default depth for recursive custom validation.
	ValidatorMaxDepthLimit         = 128     // Maximum supported custom-validation depth.
	DefaultMaxBodyBytes      int64 = 1 << 20 // Default request body limit: 1 MiB.
)

// Config contains instance-local policies. Its zero value selects safe defaults.
// Custom binders/validators own their corresponding size/depth policies.
type Config struct {
	ValidatorMaxDepth  int                 // Zero selects DefaultValidatorMaxDepth.
	MaxBodyBytes       int64               // Zero selects DefaultMaxBodyBytes; negative values are invalid.
	Binder             IBinder             // Nil selects the source-isolated Binder.
	Validator          IRequestValidator   // Nil selects recursive IValidator validation.
	RequestErrorMapper IRequestErrorMapper // Nil returns only the matching HTTP status.
	TransformResponse  ResponseTransformer // Optional default-JSON body transformation.
	TokenExtractor     ITokenExtractor     // Nil selects BearerTokenExtractor; extraction is not authentication.
}

// normalize validates default policies without mutating caller-owned configuration.
func (c Config) normalize() (Config, error) {
	if isNilInterface(c.Validator) {
		if c.ValidatorMaxDepth < 0 || c.ValidatorMaxDepth > ValidatorMaxDepthLimit {
			return c, fmt.Errorf("xgin: validator max depth must be between 0 and %d", ValidatorMaxDepthLimit)
		}
		if c.ValidatorMaxDepth == 0 {
			c.ValidatorMaxDepth = DefaultValidatorMaxDepth
		}
	}
	if isNilInterface(c.Binder) {
		if c.MaxBodyBytes < 0 {
			return c, fmt.Errorf("xgin: max body bytes cannot be negative")
		}
		if c.MaxBodyBytes == 0 {
			c.MaxBodyBytes = DefaultMaxBodyBytes
		}
	}
	return c, nil
}
