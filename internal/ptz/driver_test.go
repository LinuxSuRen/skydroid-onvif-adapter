package ptz

import (
	"testing"
	"time"
)

func TestEstimatorIntegration(t *testing.T) {
	e := &estimator{last: time.Now().Add(-time.Second)}
	e.panVel = 1 // 满速 90°/s
	e.advance()
	if e.pan < 80 || e.pan > 100 {
		t.Fatalf("pan = %v, want ~90", e.pan)
	}
}

func TestEstimatorClamp(t *testing.T) {
	e := &estimator{last: time.Now().Add(-time.Hour)}
	e.panVel = 1
	e.tiltVel = -1
	e.advance()
	if e.pan != PanMax || e.tilt != TiltMin {
		t.Fatalf("clamp: pan=%v tilt=%v", e.pan, e.tilt)
	}
}
