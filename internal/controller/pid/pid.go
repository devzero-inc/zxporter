package pid

import (
	"math"
	"time"
)

const (
	minFrequency = 30 * time.Second
	maxFrequency = 20 * time.Minute
	windowSize   = 15 // Window size for the sliding window
)

// AntiWindupController with sliding window for integral term
type AntiWindupController struct {
	Config      AntiWindupControllerConfig
	State       AntiWindupControllerState
	errorWindow []float64 // Sliding window of historical errors
}

type AntiWindupControllerConfig struct {
	ProportionalGain              float64
	IntegralGain                  float64
	DerivativeGain                float64
	AntiWindUpGain                float64
	IntegralDischargeTimeConstant float64
	LowPassTimeConstant           time.Duration
	MaxOutput                     float64
	MinOutput                     float64
}

type AntiWindupControllerState struct {
	ControlError             float64
	ControlErrorIntegral     float64
	ControlErrorDerivative   float64
	ControlSignal            float64
	UnsaturatedControlSignal float64
}

// Initialize the AntiWindupController
func NewAntiWindupController(config AntiWindupControllerConfig) *AntiWindupController {
	return &AntiWindupController{
		Config:      config,
		errorWindow: make([]float64, 0, windowSize),
	}
}

// Update the controller state with a new error
func (c *AntiWindupController) Update(input AntiWindupControllerInput) {
	// Add new error to the window
	c.errorWindow = append(c.errorWindow, input.ReferenceSignal-input.ActualSignal)

	// Maintain window size
	if len(c.errorWindow) > windowSize {
		c.errorWindow = c.errorWindow[1:] // Remove the oldest error
	}

	// Calculate windowed integral
	windowIntegral := 0.0
	for _, err := range c.errorWindow {
		windowIntegral += err * input.SamplingInterval.Seconds()
	}

	// Calculate derivative (low-pass filtered)
	derivative := ((1/c.Config.LowPassTimeConstant.Seconds())*(c.errorWindow[len(c.errorWindow)-1]-c.State.ControlError) +
		c.State.ControlErrorDerivative) / (input.SamplingInterval.Seconds()/c.Config.LowPassTimeConstant.Seconds() + 1)

	// Calculate control signal
	unsaturated := c.Config.ProportionalGain*c.errorWindow[len(c.errorWindow)-1] +
		c.Config.IntegralGain*windowIntegral + // Use windowed integral
		c.Config.DerivativeGain*derivative +
		input.FeedForwardSignal

	// Apply saturation
	saturated := math.Max(c.Config.MinOutput, math.Min(c.Config.MaxOutput, unsaturated))

	// Anti-windup: Clamp integral growth when saturated
	if saturated != unsaturated {
		windowIntegral += c.Config.AntiWindUpGain * (saturated - unsaturated)
	}

	// Update state
	c.State.ControlError = c.errorWindow[len(c.errorWindow)-1]
	c.State.ControlErrorIntegral = windowIntegral
	c.State.ControlErrorDerivative = derivative
	c.State.ControlSignal = saturated
	c.State.UnsaturatedControlSignal = unsaturated

	// // Apply integral discharge
	// c.dischargeIntegral(input.SamplingInterval)
}

// Discharge the integral term over time
func (c *AntiWindupController) dischargeIntegral(dt time.Duration) {
	decayFactor := math.Max(0, 1-dt.Seconds()/c.Config.IntegralDischargeTimeConstant)
	for i := range c.errorWindow {
		c.errorWindow[i] *= decayFactor
	}
}

// Reset the controller state
func (c *AntiWindupController) Reset() {
	c.errorWindow = make([]float64, 0, windowSize)
	c.State = AntiWindupControllerState{}
}

type AntiWindupControllerInput struct {
	ReferenceSignal   float64
	ActualSignal      float64
	FeedForwardSignal float64
	SamplingInterval  time.Duration
}
