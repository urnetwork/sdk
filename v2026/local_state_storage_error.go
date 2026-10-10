package sdk

// Checked FFI boundaries expose a stable stage, never a filesystem path or
// rejected stored value. Go callers retain errors.Is/As for the underlying
// failure; native diagnostics must use the public stage, not unwrap content.
type localStateStorageError struct {
	stage string
	cause error
}

func (self *localStateStorageError) Error() string { return self.stage }
func (self *localStateStorageError) Unwrap() error { return self.cause }

func localStorageStageError(stage string, cause error) error {
	if cause == nil {
		return nil
	}
	return &localStateStorageError{stage: stage, cause: cause}
}
