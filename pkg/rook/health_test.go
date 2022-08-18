package rook

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/replicatedhq/kurl/pkg/rook/cephtypes"
	"github.com/replicatedhq/kurl/pkg/rook/testfiles"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
	restclient "k8s.io/client-go/rest"
)

func Test_isStatusHealthy(t *testing.T) {
	tests := []struct {
		name    string
		status  []byte
		health  bool
		message string
	}{
		{
			name:    "healthy ceph",
			status:  testfiles.HealthyCephStatus1,
			health:  true,
			message: "",
		},
		{
			name:    "ceph finished rebalancing",
			status:  testfiles.RebalanceCephStatus1,
			health:  true,
			message: "",
		},
		{
			name:    "ceph rebalancing",
			status:  testfiles.RebalanceCephStatus2,
			health:  false,
			message: "health is HEALTH_WARN not HEALTH_OK and 1099 bytes are being recovered per second, 0 desired and 0.142857% of PGs are inactive, 0.181073% are degraded, and 64.709807% are misplaced, 0 required for all",
		},
		{
			name:    "ceph health_err due to full osd", // this message could very much be improved
			status:  testfiles.RebalanceCephStatusFull,
			health:  false,
			message: "health is HEALTH_ERR not HEALTH_OK and 0.000000% of PGs are inactive, 0.516218% are degraded, and 0.000000% are misplaced, 0 required for all",
		},
		{
			name:    "ceph rebalancing multinode",
			status:  testfiles.RebalanceCephStatusMultinode,
			health:  false,
			message: "health is HEALTH_WARN not HEALTH_OK and 18863356 bytes are being recovered per second, 0 desired and 0.000000% of PGs are inactive, 42.455066% are degraded, and 2.081463% are misplaced, 0 required for all",
		},
		{
			name:    "ceph has too many PGs per OSD",
			status:  testfiles.TooManyPGSPerOSD,
			health:  true,
			message: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := require.New(t)
			cephStatus := cephtypes.CephStatus{}
			err := json.Unmarshal(tt.status, &cephStatus)
			req.NoError(err)

			gotHealth, gotMessage := isStatusHealthy(cephStatus)
			req.Equal(tt.health, gotHealth)
			req.Equal(tt.message, gotMessage)
		})
	}
}

func Test_parseSafeToRemoveOSD(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    bool
		wantPgs int
	}{
		{
			name:    "osd 6 not safe to remove 4pgs",
			output:  "Error EBUSY: OSD(s) 6 have 4 pgs currently mapped to them.",
			want:    false,
			wantPgs: 4,
		},
		{
			name:    "osd 6 not safe to remove 49pgs",
			output:  "Error EBUSY: OSD(s) 6 have 49 pgs currently mapped to them.",
			want:    false,
			wantPgs: 49,
		},
		{
			name:   "osd 6 is safe to remove",
			output: "OSD(s) 6 are safe to destroy without reducing data durability.",
			want:   true,
		},
		{
			name:   "osd 0 is safe to remove",
			output: "OSD(s) 0 are safe to destroy without reducing data durability.",
			want:   true,
		},
		{
			name:    "unparseable content",
			output:  "this is some other format of message",
			want:    false,
			wantPgs: -1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := require.New(t)
			got, pgs := parseSafeToRemoveOSD(tt.output)
			req.Equal(tt.want, got)
			req.Equal(tt.wantPgs, pgs)
		})
	}
}

func Test_waitForOkToRemoveOSD(t *testing.T) {
	loopSleep = time.Millisecond * 20
	backgroundComplete := false
	tests := []struct {
		name           string
		osdToRemove    int64
		responses      execResponses
		backgroundFunc func()
	}{
		{
			name: "basic progression",
			responses: map[string]struct {
				errcode        int
				stdout, stderr string
				err            error
			}{
				`ceph - status - --format - json-pretty - rook-ceph - rook-ceph-tools-785466cbdd-wk8rx - rook-ceph-tools`: {
					stdout: string(testfiles.RebalanceCephStatusMultinode),
				},
			},
			osdToRemove: 4,
			backgroundFunc: func() {
				time.Sleep(time.Millisecond * 100)

				// start returning a healthy status, and a 'not ok to remove osd 4' response
				setToolboxExecFunc(map[string]struct {
					errcode        int
					stdout, stderr string
					err            error
				}{
					`ceph - status - --format - json-pretty - rook-ceph - rook-ceph-tools-785466cbdd-wk8rx - rook-ceph-tools`: {
						stdout: string(testfiles.HealthyCephStatus1),
					},
					`ceph - osd - safe-to-destroy - osd.4 - rook-ceph - rook-ceph-tools-785466cbdd-wk8rx - rook-ceph-tools`: {
						stderr: "Error EBUSY: OSD(s) 4 have 49 pgs currently mapped to them.",
					},
				})

				time.Sleep(time.Millisecond * 100)

				// ok to remove
				setToolboxExecFunc(map[string]struct {
					errcode        int
					stdout, stderr string
					err            error
				}{
					`ceph - status - --format - json-pretty - rook-ceph - rook-ceph-tools-785466cbdd-wk8rx - rook-ceph-tools`: {
						stdout: string(testfiles.HealthyCephStatus1),
					},
					`ceph - osd - safe-to-destroy - osd.4 - rook-ceph - rook-ceph-tools-785466cbdd-wk8rx - rook-ceph-tools`: {
						stderr: "OSD(s) 4 are safe to destroy without reducing data durability.",
					},
				})
				backgroundComplete = true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := require.New(t)
			clientset := fake.NewSimpleClientset(append(runtimeFromPodlistJson(testfiles.SixBlockDevicePods), runtimeFromDeploymentlistJson(testfiles.Rook6OSDDeployments)...)...)
			InitWriter(testWriter{t: t})

			testCtx, cancelfunc := context.WithTimeout(context.Background(), time.Minute) // if your test takes more than 1m, there are issues
			defer cancelfunc()
			setToolboxExecFunc(tt.responses)
			conf = &restclient.Config{} // set the rest client so that runToolboxCommand does not attempt to fetch it

			if tt.backgroundFunc != nil {
				go tt.backgroundFunc()
			} else {
				backgroundComplete = true
			}

			err := waitForOkToRemoveOSD(testCtx, clientset, tt.osdToRemove)
			req.NoError(err)

			req.Equal(true, backgroundComplete) // the background function should have marked this as complete
		})
	}
}