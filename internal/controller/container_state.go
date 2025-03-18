package controller

import (
	"github.com/devzero-inc/zxporter/internal/controller/pid"
)

func NewContainerState(namespace, podName, containerName string) *ContainerState {
	return &ContainerState{
		Namespace:     namespace,
		PodName:       podName,
		ContainerName: containerName,
		PIDController: &pid.AntiWindupController{
			// TODO: consider finding an optimal tuning path for these values
			Config: pid.AntiWindupControllerConfig{
				ProportionalGain:              ProportionalGain, // Need to tune these values correctly
				IntegralGain:                  IntegralGain,     // Look at the bayes theroem
				DerivativeGain:                DerivativeGain,
				AntiWindUpGain:                AntiWindUpGain,
				IntegralDischargeTimeConstant: IntegralDischargeTimeConstant,
				LowPassTimeConstant:           LowPassTimeConstant,
				MaxOutput:                     MaxOutput,
				MinOutput:                     MinOutput,
			},
		},
		Frequency:     defaultFrequency,
		UpdateChan:    make(chan struct{}, 1),
		ContainerQuit: make(chan bool),
		Reconciling:   false,
	}
}
