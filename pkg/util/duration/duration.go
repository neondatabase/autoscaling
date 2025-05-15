package duration

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/neondatabase/autoscaling/pkg/util"
)

type Duration struct {
	Seconds uint `json:"seconds"`
}

func NewDuration(d time.Duration) Duration {
	return Duration{
		Seconds: uint(d.Seconds()),
	}
}

func (d Duration) ToDuration() time.Duration {
	return time.Second * time.Duration(d.Seconds)
}

func (d Duration) String() string {
	return fmt.Sprintf("%ds", d.Seconds)
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Seconds)
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var seconds uint
	if err := json.Unmarshal(data, &seconds); err != nil {
		return err
	}
	d.Seconds = seconds
	return nil
}

type RandomDuration struct {
	MinSeconds uint `json:"minSeconds"`
	MaxSeconds uint `json:"maxSeconds"`
}

func NewRandomDuration(min, max time.Duration) RandomDuration {
	return RandomDuration{
		MinSeconds: uint(min.Seconds()),
		MaxSeconds: uint(max.Seconds()),
	}
}

func (d RandomDuration) ToTimeRange() *util.TimeRange {
	return util.NewTimeRange(time.Second, int(d.MinSeconds), int(d.MaxSeconds))
}

func (d RandomDuration) Random() time.Duration {
	return d.ToTimeRange().Random()
}

func (d RandomDuration) String() string {
	return fmt.Sprintf("%ds-%ds", d.MinSeconds, d.MaxSeconds)
}

func (d RandomDuration) Validate() error {
	if d.MinSeconds == 0 && d.MaxSeconds == 0 {
		return fmt.Errorf("both min and max seconds cannot be zero")
	}
	if d.MaxSeconds < d.MinSeconds {
		return fmt.Errorf("max seconds (%d) cannot be less than min seconds (%d)", d.MaxSeconds, d.MinSeconds)
	}
	return nil
}

func (d RandomDuration) MarshalJSON() ([]byte, error) {
	type Alias RandomDuration
	return json.Marshal(&struct {
		Alias
	}{
		Alias: Alias(d),
	})
}

func (d *RandomDuration) UnmarshalJSON(data []byte) error {
	type Alias RandomDuration
	aux := &struct {
		Alias
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*d = RandomDuration(aux.Alias)
	return d.Validate()
}
