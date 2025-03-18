package controller

// affects the rate with with data is sampled and considered.
const (
	// _ENV_CP_OVERRIDE_NAMESPACE_EXCLUSIONS is a comma-separated list of namespaces that the operator will exclude from it's reporting.
	// This value will be obtained from the control plane (eg: pulse.devzero.io). If this environment variable is set, it will ignore
	// the data sent from the control plane, and just use these values.
	// Default value: []
	_ENV_CP_OVERRIDE_NAMESPACE_EXCLUSIONS = "CP_OVERRIDE_NAMESPACE_EXCLUSIONS"
)

// affects the PID controllers
const (
	// PROPORTIONAL_GAIN sets the proportional gain for the control loop.
	// This value will be obtained from the control plane (eg: pulse.devzero.io). If this environment variable is set, it will ignore
	// the data sent from the control plane, and just use this value.
	// Default value: 8.0
	_ENV_CP_PROPORTIONAL_GAIN = "PROPORTIONAL_GAIN"

	// INTEGRAL_GAIN sets the integral gain for the control loop.
	// This value will be obtained from the control plane (eg: pulse.devzero.io). If this environment variable is set, it will ignore
	// the data sent from the control plane, and just use this value.
	// Default value: 0.3
	_ENV_CP_INTEGRAL_GAIN = "INTEGRAL_GAIN"

	// DERIVATIVE_GAIN sets the derivative gain for the control loop.
	// This value will be obtained from the control plane (eg: pulse.devzero.io). If this environment variable is set, it will ignore
	// the data sent from the control plane, and just use this value.
	// Default value: 1.0
	_ENV_CP_DERIVATIVE_GAIN = "DERIVATIVE_GAIN"

	// ANTI_WINDUP_GAIN sets the gain for anti-windup compensation.
	// This value will be obtained from the control plane (eg: pulse.devzero.io). If this environment variable is set, it will ignore
	// the data sent from the control plane, and just use this value.
	// Default value: 1.0
	_ENV_CP_ANTI_WINDUP_GAIN = "ANTI_WINDUP_GAIN"

	// INTEGRAL_DISCHARGE_TIME_CONSTANT sets the discharge time constant for the integral term.
	// This value will be obtained from the control plane (eg: pulse.devzero.io). If this environment variable is set, it will ignore
	// the data sent from the control plane, and just use this value.
	// Default value: 1.0
	_ENV_CP_INTEGRAL_DISCHARGE_TIME_CONSTANT = "INTEGRAL_DISCHARGE_TIME_CONSTANT"

	// LOW_PASS_TIME_CONSTANT sets the time constant for the low-pass filter.
	// This value will be obtained from the control plane (eg: pulse.devzero.io). If this environment variable is set, it will ignore
	// the data sent from the control plane, and just use this value.
	// Default value: 2 seconds
	_ENV_CP_LOW_PASS_TIME_CONSTANT = "LOW_PASS_TIME_CONSTANT"

	// MAX_OUTPUT sets the maximum output limit.
	// This value will be obtained from the control plane (eg: pulse.devzero.io). If this environment variable is set, it will ignore
	// the data sent from the control plane, and just use this value.
	// Default value: 1.01
	_ENV_CP_MAX_OUTPUT = "MAX_OUTPUT"

	// MIN_OUTPUT sets the minimum output limit.
	// This value will be obtained from the control plane (eg: pulse.devzero.io). If this environment variable is set, it will ignore
	// the data sent from the control plane, and just use this value.
	// Default value: -2.15
	_ENV_CP_MIN_OUTPUT = "MIN_OUTPUT"
)
