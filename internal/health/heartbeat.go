package health

import (
	"time"

	gen "github.com/devzero-inc/zxporter/gen/api/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// toProtoHealthStatus maps internal HealthStatus to proto HealthStatus.
func toProtoHealthStatus(s HealthStatus) gen.HealthStatus {
	switch s {
	case HealthStatusHealthy:
		return gen.HealthStatus_HEALTH_STATUS_HEALTHY
	case HealthStatusDegraded:
		return gen.HealthStatus_HEALTH_STATUS_DEGRADED
	case HealthStatusUnhealthy:
		return gen.HealthStatus_HEALTH_STATUS_UNHEALTHY
	default:
		return gen.HealthStatus_HEALTH_STATUS_UNSPECIFIED
	}
}

// worstStatus returns the more severe of two HealthStatus values.
// NOTE: This relies on proto enum values being ordered by increasing severity:
// UNSPECIFIED(0) < HEALTHY(1) < DEGRADED(2) < UNHEALTHY(3).
func worstStatus(a, b gen.HealthStatus) gen.HealthStatus {
	if a > b {
		return a
	}
	return b
}

// BuildHeartbeatRequest constructs a ReportHealthRequest from the current
// HealthManager state. The zxporter operator type is OPERATOR_TYPE_READ.
func BuildHeartbeatRequest(hm *HealthManager, clusterID, version, commit string, startTime time.Time) *gen.ReportHealthRequest {
	return BuildHeartbeatRequestFromReport(hm.BuildReport(), clusterID, version, commit, startTime)
}

// BuildHeartbeatRequestFromReport constructs a ReportHealthRequest from an
// already-built report map. Use this when you need to log and send the same
// snapshot to avoid a double lock acquisition on HealthManager.
func BuildHeartbeatRequestFromReport(report map[string]ComponentStatus, clusterID, version, commit string, startTime time.Time) *gen.ReportHealthRequest {
	overall := gen.HealthStatus_HEALTH_STATUS_UNSPECIFIED
	components := make([]*gen.ComponentHealth, 0, len(report))

	for name, cs := range report {
		protoStatus := toProtoHealthStatus(cs.Status)
		overall = worstStatus(overall, protoStatus)

		components = append(components, &gen.ComponentHealth{
			Name:     name,
			Status:   protoStatus,
			Message:  cs.Message,
			Metadata: cs.Metadata,
		})
	}

	return &gen.ReportHealthRequest{
		ClusterId:     clusterID,
		OperatorType:  gen.OperatorType_OPERATOR_TYPE_READ,
		Version:       version,
		Commit:        commit,
		OverallStatus: overall,
		Components:    components,
		UptimeSince:   timestamppb.New(startTime),
	}
}
