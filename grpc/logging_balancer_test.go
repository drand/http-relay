package grpc

import (
	"errors"
	"testing"

	"google.golang.org/grpc/balancer"
)

// mockLogger is a simple logger implementation for testing
type mockLogger struct {
	errors []string
	debugs []string
}

func (m *mockLogger) Error(msg string, args ...any) {
	m.errors = append(m.errors, msg)
}

func (m *mockLogger) Warn(msg string, args ...any) {
	// Not used in tests
}

func (m *mockLogger) Info(msg string, args ...any) {
	// Not used in tests
}

func (m *mockLogger) Debug(msg string, args ...any) {
	m.debugs = append(m.debugs, msg)
}

// errorPicker is a picker that always returns an error
type errorPicker struct {
	err error
}

func (p *errorPicker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	return balancer.PickResult{}, p.err
}

// successPicker is a picker that always succeeds
type successPicker struct {
	result balancer.PickResult
}

func (p *successPicker) Pick(balancer.PickInfo) (balancer.PickResult, error) {
	return p.result, nil
}

func TestLogPicker_Pick_ErrorHandling(t *testing.T) {
	tests := []struct {
		name        string
		picker      balancer.Picker
		expectError bool
		errorMsg    string
	}{
		{
			name:        "picker returns error",
			picker:      &errorPicker{err: balancer.ErrNoSubConnAvailable},
			expectError: true,
			errorMsg:    "no SubConn available",
		},
		{
			name:        "picker returns custom error",
			picker:      &errorPicker{err: errors.New("connection failed")},
			expectError: true,
			errorMsg:    "connection failed",
		},
		{
			name:        "picker succeeds",
			picker:      &successPicker{result: balancer.PickResult{}},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := &mockLogger{}
			picker := &logPicker{
				sub: tt.picker,
				log: logger,
			}

			result, err := picker.Pick(balancer.PickInfo{
				FullMethodName: "test.Method",
			})

			if tt.expectError {
				if err == nil {
					t.Fatal("expected error but got nil")
				}
				if err.Error() != tt.errorMsg && tt.errorMsg != "" {
					// For ErrNoSubConnAvailable, check it's the right type
					if !errors.Is(err, balancer.ErrNoSubConnAvailable) && tt.errorMsg == "no SubConn available" {
						t.Errorf("expected ErrNoSubConnAvailable, got %v", err)
					}
				}
				// Verify result is zero-valued when error occurs
				if result.SubConn != nil {
					t.Error("expected zero-valued result when error occurs")
				}
				// Verify we logged the error
				if len(logger.debugs) == 0 {
					t.Error("expected debug log for error case")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				// Verify we logged the success
				if len(logger.debugs) == 0 {
					t.Error("expected debug log for success case")
				}
			}
		})
	}
}

func TestLogPicker_Pick_NoPanicOnError(t *testing.T) {
	// This test specifically verifies that accessing result fields
	// when an error is returned doesn't cause a panic
	picker := &logPicker{
		sub: &errorPicker{err: balancer.ErrNoSubConnAvailable},
		log: &mockLogger{},
	}

	// This should not panic even though result is zero-valued
	result, err := picker.Pick(balancer.PickInfo{
		FullMethodName: "test.Method",
	})

	if err == nil {
		t.Fatal("expected error")
	}

	// Accessing result fields should be safe (they should be zero-valued)
	_ = result.SubConn
	_ = result.Metadata
	_ = result.Done
}

func TestLogPicker_Pick_DoneCallback(t *testing.T) {
	doneCalled := false
	doneErr := errors.New("done error")

	picker := &logPicker{
		sub: &successPicker{
			result: balancer.PickResult{
				Done: func(info balancer.DoneInfo) {
					doneCalled = true
					if info.Err != nil && info.Err.Error() != doneErr.Error() {
						t.Errorf("unexpected error in Done callback: %v", info.Err)
					}
				},
			},
		},
		log: &mockLogger{},
	}

	result, err := picker.Pick(balancer.PickInfo{
		FullMethodName: "test.Method",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Done == nil {
		t.Fatal("expected Done callback to be set")
	}

	// Call Done to verify it works
	result.Done(balancer.DoneInfo{
		Err: doneErr,
	})

	if !doneCalled {
		t.Error("expected Done callback to be called")
	}
}

func TestLogPicker_Pick_DoneCallbackNil(t *testing.T) {
	// Test that we handle nil Done callback gracefully
	picker := &logPicker{
		sub: &successPicker{
			result: balancer.PickResult{
				// Done is nil
			},
		},
		log: &mockLogger{},
	}

	result, err := picker.Pick(balancer.PickInfo{
		FullMethodName: "test.Method",
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Our wrapper should handle nil Done gracefully
	if result.Done != nil {
		// Calling it should not panic even if underlying Done was nil
		result.Done(balancer.DoneInfo{
			Err: errors.New("test error"),
		})
	}
}
