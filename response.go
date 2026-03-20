package xgin

type ContentType int8

const (
	ContentTypeJSON ContentType = iota
	ContentTypeIndentedJSON
	ContentTypeSecureJSON
	ContentTypeJsonpJSON
	ContentTypeAsciiJSON
	ContentTypePureJSON
	ContentTypeMsgPack
	ContentTypeProtoBuf
	ContentTypeRedirect
	ContentTypeString
	ContentTypeTOML
	ContentTypeXML
	ContentTypeYAML
	ContentTypeHTML
)

type Response struct {
	HttpStatus  int               `json:"-"`
	Code        int               `json:"code"`
	Msg         string            `json:"msg"`
	Data        any               `json:"data"`
	Header      map[string]string `json:"-"`
	Errors      []error           `json:"-"`
	ContentType ContentType       `json:"-"`
	HtmlPath    string            `json:"-"`
}

func (r Response) WithHttpStatus(status int) Response {
	r.HttpStatus = status
	return r
}

func (r Response) WithCode(code int) Response {
	r.Code = code
	return r
}

func (r Response) WithMsg(msg string) Response {
	r.Msg = msg
	return r
}

func (r Response) WithData(data any) Response {
	r.Data = data
	return r
}

func (r Response) WithHeader(key, val string) Response {
	headers := make(map[string]string, len(r.Header)+1)
	for k, v := range r.Header {
		headers[k] = v
	}
	headers[key] = val
	r.Header = headers
	return r
}

func (r Response) WithHeaders(kv map[string]string) Response {
	if len(kv) == 0 {
		return r
	}
	headers := make(map[string]string, len(r.Header)+len(kv))
	for k, v := range r.Header {
		headers[k] = v
	}
	for k, v := range kv {
		headers[k] = v
	}
	r.Header = headers
	return r
}

func (r Response) WithError(err error) Response {
	if err == nil {
		return r
	}
	newErrs := make([]error, len(r.Errors)+1)
	copy(newErrs, r.Errors)
	newErrs[len(r.Errors)] = err
	r.Errors = newErrs
	return r
}

func (r Response) WithErrors(errors ...error) Response {
	if len(errors) == 0 {
		return r
	}
	errs := make([]error, len(r.Errors)+len(errors))
	copy(errs, r.Errors)
	copy(errs[len(r.Errors):], errors)
	r.Errors = errs
	return r
}

func (r Response) WithContentType(typ ContentType) Response {
	r.ContentType = typ
	return r
}

func (r Response) WithHtmlPath(htmlPath string) Response {
	r.HtmlPath = htmlPath
	return r
}
