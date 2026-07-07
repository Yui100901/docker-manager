package diagnostics

import (
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/container"
)

type logDriverAvailability struct {
	Driver   string
	Status   string
	Readable bool
	Reason   string
}

func inspectLogDriver(inspect container.InspectResponse) string {
	if inspect.HostConfig == nil {
		return ""
	}
	return strings.TrimSpace(inspect.HostConfig.LogConfig.Type)
}

func containerLogDriverAvailability(inspect container.InspectResponse) logDriverAvailability {
	return logDriverAvailabilityForDriver(inspectLogDriver(inspect))
}

func logDriverAvailabilityForDriver(driver string) logDriverAvailability {
	driver = strings.ToLower(strings.TrimSpace(driver))
	if driver == "" {
		return logDriverAvailability{
			Status:   "unknown",
			Readable: true,
			Reason:   "log driver is unavailable from inspect; docker logs will be attempted",
		}
	}
	switch driver {
	case "json-file", "local", "journald":
		return logDriverAvailability{
			Driver:   driver,
			Status:   "supported",
			Readable: true,
			Reason:   fmt.Sprintf("docker logs supports the %s log driver", driver),
		}
	case "none":
		return logDriverAvailability{
			Driver:   driver,
			Status:   "disabled",
			Readable: false,
			Reason:   "container logging is disabled by the none log driver",
		}
	case "syslog", "gelf", "fluentd", "awslogs", "splunk", "etwlogs", "gcplogs", "logentries":
		return logDriverAvailability{
			Driver:   driver,
			Status:   "unsupported",
			Readable: false,
			Reason:   fmt.Sprintf("docker logs does not read logs from the %s log driver", driver),
		}
	default:
		return logDriverAvailability{
			Driver:   driver,
			Status:   "unknown",
			Readable: true,
			Reason:   fmt.Sprintf("log driver %s is not in the built-in support list; docker logs will be attempted", driver),
		}
	}
}
