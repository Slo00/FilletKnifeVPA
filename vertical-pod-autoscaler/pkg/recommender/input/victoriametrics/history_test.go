/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package victoriametrics

import (
	"encoding/json"
	"math"
	"testing"

	prommodel "github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToSeriesDropsWhatCannotBeDrawn(t *testing.T) {
	got := toSeries([]prommodel.SamplePair{
		{Timestamp: 1_000_000, Value: 1},
		{Timestamp: 1_030_000, Value: prommodel.SampleValue(math.NaN())},
		{Timestamp: 1_060_000, Value: prommodel.SampleValue(math.Inf(1))},
		{Timestamp: 1_090_000, Value: 2.5},
	})
	assert.Equal(t, Series{{1000, 1}, {1090, 2.5}}, got)
	assert.Empty(t, toSeries(nil))
}

func TestHistoryMarshalsAsPairs(t *testing.T) {
	raw, err := json.Marshal(History{CPU: ResourceHistory{Usage: Series{{1000, 0.5}}}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"cpu":{"usage":[[1000,0.5]],"requests":null,"limits":null},"memory":{"usage":null,"requests":null,"limits":null}}`, string(raw))
}
