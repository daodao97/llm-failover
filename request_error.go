package failover

// RequestError 让 GetChannels 等宿主回调直接声明最终返回给客户端的错误类型与状态码，
// 而不是由库按错误文本推断。
type RequestError struct {
	Type       string
	StatusCode int
	Err        error
}

// NewRequestError 构建带错误类型与状态码的错误。
func NewRequestError(errType string, statusCode int, err error) *RequestError {
	return &RequestError{Type: errType, StatusCode: statusCode, Err: err}
}

func (e *RequestError) Error() string { return e.Err.Error() }
func (e *RequestError) Unwrap() error { return e.Err }
