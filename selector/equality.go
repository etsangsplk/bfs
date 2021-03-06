package selector

import (
	"bfs/util/logging"
	"github.com/golang/glog"
)

type EqualsPredicate struct {
	Key   string
	Value string
}

func (this *EqualsPredicate) Evaluate(key string, value string) bool {
	glog.V(logging.LogLevelTrace).Infof("Evaluate equality expression: %s %#v against label: %s %s", this.Key, this.Value, key, value)

	if this.Key == key {
		if this.Value == "" || this.Value == value {
			return true
		}
	}

	return false
}
