package xgin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

// ---------------------------------------------------------------------------------------------------------------------

var (
	// ErrInvalidBindTarget reports a target that is not a non-nil pointer to a struct.
	ErrInvalidBindTarget = errors.New("xgin: bind target must be a non-nil pointer to a struct")

	// ErrMissingContentType reports a missing Content-Type for body binding.
	ErrMissingContentType = errors.New("xgin: request body content type is required")

	// ErrUnsupportedContentType reports an invalid, ambiguous, or unsupported Content-Type.
	ErrUnsupportedContentType = errors.New("xgin: unsupported request body content type")

	// ErrInvalidJSONBody reports a body that is not one complete JSON document.
	ErrInvalidJSONBody = errors.New("xgin: invalid JSON body")
)

// bindSource identifies the HTTP source assigned to a shadow DTO.
type bindSource uint8

const (
	bindSourceURI bindSource = iota + 1
	bindSourceHeader
	bindSourceQuery
	bindSourceForm
	bindSourceFile
	bindSourceBody
)

// bodyFormat identifies the encodings shared by all body fields.
type bodyFormat uint8

const (
	bodyFormatJSON bodyFormat = 1 << iota
	bodyFormatXML
	bodyFormatYAML
	bodyFormatTOML
	bodyFormatMsgpack
	bodyFormatBSON
	bodyFormatForm
)

var fileHeaderType = reflect.TypeFor[multipart.FileHeader]()

const allBodyFormats = bodyFormatJSON |
	bodyFormatXML |
	bodyFormatYAML |
	bodyFormatTOML |
	bodyFormatMsgpack |
	bodyFormatBSON |
	bodyFormatForm

// bindingPlanEntry caches a compilation result, including errors, with sync.Once.
type bindingPlanEntry struct {
	once sync.Once
	plan *bindingPlan
	err  error
}

// shadowField retains a field and its full path through flattened embedded structs.
type shadowField struct {
	field      reflect.StructField
	targetPath []int
}

// fieldMapping maps a shadow field to its original DTO field.
type fieldMapping struct {
	shadowIndex int
	targetPath  []int
}

// ---------------------------------------------------------------------------------------------------------------------

// Binder binds isolated HTTP sources using cached plans and Gin validation.
// Plans remain cached for the instance lifetime; use a bounded set of DTO types.
// Do not copy a Binder after first use.
type Binder struct {
	plans        sync.Map
	maxBodyBytes int64
}

// NewBinder creates a binder with a positive body limit.
// Streaming uploads require a separate handler.
func NewBinder(maxBodyBytes int64) (*Binder, error) {
	if maxBodyBytes <= 0 {
		return nil, errors.New("xgin: max body bytes must be positive")
	}

	return &Binder{maxBodyBytes: maxBodyBytes}, nil
}

// Prepare compiles and caches a non-nil struct pointer before serving requests.
// Body fields must share at least one supported encoding.
func (b *Binder) Prepare(target any) error {
	_, targetType, err := b.targetOf(target)
	if err != nil {
		return err
	}

	_, err = b.planFor(targetType)
	return err
}

// Bind fills a non-nil struct pointer and validates each source with Gin.
// Use a zero-value DTO; absent tagged fields receive zero values or tag defaults.
// Named nested fields require source tags, including nested form fields.
// Failures leave target unchanged. Cross-source rules belong in IValidator.
func (b *Binder) Bind(c *gin.Context, target any) error {
	if c == nil {
		return errors.New("xgin: gin context cannot be nil")
	}

	if c.Request == nil {
		return errors.New("xgin: gin context has no request")
	}

	if c.Request.URL == nil {
		return errors.New("xgin: request URL cannot be nil")
	}

	targetValue, targetType, err := b.targetOf(target)
	if err != nil {
		return err
	}

	plan, err := b.planFor(targetType)
	if err != nil {
		return err
	}

	contentType, err := plan.contentType(c)
	if err != nil {
		return err
	}

	if err = b.bufferBody(c, plan, contentType); err != nil {
		return err
	}

	candidate := reflect.New(targetType).Elem()
	candidate.Set(targetValue)

	if err = plan.bind(c, candidate, contentType); err != nil {
		return err
	}

	targetValue.Set(candidate)
	return nil
}

// bufferBody bounds the entire body before decoding and checks complete JSON documents.
func (b *Binder) bufferBody(c *gin.Context, plan *bindingPlan, contentType string) error {
	isReadOnly := c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead
	if isReadOnly || c.Request.Body == nil || c.Request.Body == http.NoBody {
		return nil
	}

	readsForm := plan.form.enabled() && plan.form.isFormRequest(contentType)
	readsFiles := plan.file.enabled() && contentType == binding.MIMEMultipartPOSTForm
	if !plan.body.enabled() && !readsForm && !readsFiles {
		return nil
	}

	limit := b.maxBodyBytes
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, limit)

	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()

	if readErr != nil {
		return fmt.Errorf("xgin: read request body: %w", readErr)
	}

	if closeErr != nil {
		return fmt.Errorf("xgin: close request body: %w", closeErr)
	}

	// Reading the entire bounded body also covers decoder-ignored suffixes and
	// unknown content lengths; validating JSON prevents accepting a second document.
	if contentType == binding.MIMEJSON && !json.Valid(body) {
		return ErrInvalidJSONBody
	}

	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	return nil
}

// targetOf returns the writable struct value and its type.
func (b *Binder) targetOf(target any) (reflect.Value, reflect.Type, error) {
	value := reflect.ValueOf(target)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return reflect.Value{}, nil, ErrInvalidBindTarget
	}

	value = value.Elem()
	if value.Kind() != reflect.Struct {
		return reflect.Value{}, nil, ErrInvalidBindTarget
	}

	return value, value.Type(), nil
}

// planFor compiles once per struct type and caches both successful plans and errors.
func (b *Binder) planFor(requestType reflect.Type) (*bindingPlan, error) {
	if requestType == nil || requestType.Kind() != reflect.Struct {
		return nil, ErrInvalidBindTarget
	}

	entry := b.planEntryFor(requestType)

	entry.once.Do(func() {
		entry.plan, entry.err = b.compilePlan(requestType)
	})

	return entry.plan, entry.err
}

// planEntryFor returns the shared compilation entry for requestType.
func (b *Binder) planEntryFor(requestType reflect.Type) *bindingPlanEntry {
	entry := &bindingPlanEntry{}
	actual, _ := b.plans.LoadOrStore(requestType, entry)
	return actual.(*bindingPlanEntry)
}

// compilePlan creates source-isolated shadows, including form fields in the query shadow.
func (b *Binder) compilePlan(requestType reflect.Type) (*bindingPlan, error) {
	plan := &bindingPlan{}
	var err error

	plan.uri, err = b.compileShadowPlan(requestType, bindSourceURI)
	if err != nil {
		return nil, err
	}

	plan.header, err = b.compileShadowPlan(requestType, bindSourceHeader)
	if err != nil {
		return nil, err
	}

	plan.query, err = b.compileShadowPlan(requestType, bindSourceQuery)
	if err != nil {
		return nil, err
	}

	plan.form, err = b.compileShadowPlan(requestType, bindSourceForm)
	if err != nil {
		return nil, err
	}

	plan.file, err = b.compileShadowPlan(requestType, bindSourceFile)
	if err != nil {
		return nil, err
	}

	plan.body, err = b.compileShadowPlan(requestType, bindSourceBody)
	if err != nil {
		return nil, err
	}

	plan.bodyFormats = b.bodyFormatsOf(plan.body)
	if plan.body.enabled() && plan.bodyFormats == 0 {
		return nil, errors.New("xgin: body fields have no common content type")
	}

	return plan, nil
}

// compileShadowPlan creates a shadow and its complete field mappings.
func (b *Binder) compileShadowPlan(requestType reflect.Type, source bindSource) (shadowPlan, error) {
	shadowFields, err := b.collectShadowFields(requestType, source, nil)
	if err != nil {
		return shadowPlan{}, err
	}

	if len(shadowFields) == 0 {
		return shadowPlan{}, nil
	}

	fields := make([]reflect.StructField, 0, len(shadowFields))
	mappings := make([]fieldMapping, 0, len(shadowFields))
	fieldNames := make(map[string]struct{}, len(shadowFields))

	for shadowIndex, entry := range shadowFields {
		if _, exists := fieldNames[entry.field.Name]; exists {
			return shadowPlan{}, fmt.Errorf(
				"xgin: duplicate embedded field %s",
				entry.field.Name,
			)
		}

		fieldNames[entry.field.Name] = struct{}{}
		fields = append(fields, entry.field)

		mappings = append(mappings, fieldMapping{
			shadowIndex: shadowIndex,
			targetPath:  entry.targetPath,
		})
	}

	// The dynamic type is created once and retained by the cached binding plan.
	return shadowPlan{
		typeOf:   reflect.StructOf(fields),
		mappings: mappings,
		source:   source,
	}, nil
}

// collectShadowFields gathers bindable fields and flattens anonymous value structs.
func (b *Binder) collectShadowFields(requestType reflect.Type, source bindSource, prefix []int) ([]shadowField, error) {
	fields := make([]shadowField, 0, requestType.NumField())

	for index := 0; index < requestType.NumField(); index++ {
		field := requestType.Field(index)

		// Unexported fields cannot be safely written and must not become HTTP parameters.
		if field.PkgPath != "" {
			continue
		}

		fieldSource, err := b.sourceOf(field)
		if err != nil {
			return nil, err
		}

		if field.Anonymous && fieldSource != 0 {
			return nil, fmt.Errorf(
				"xgin: anonymous field %s cannot declare bind tags; use a named field",
				field.Name,
			)
		}

		targetPath := append(slices.Clone(prefix), index)

		isQueryField := source == bindSourceQuery && fieldSource != bindSourceFile && b.hasBindTag(field, "form")
		if fieldSource == source || isQueryField {
			if err = b.checkFieldMapping(field, source); err != nil {
				return nil, err
			}
			fields = append(fields, shadowField{
				field:      field,
				targetPath: targetPath,
			})
			continue
		}
		if fieldSource != 0 || !field.Anonymous {
			continue
		}
		if b.ignoredEmbeddedField(field, source) {
			continue
		}
		// Flattening an anonymous pointer would require allocating its parent even
		// when no parameter was supplied, changing the DTO's nil semantics.
		nestedType := field.Type
		if nestedType.Kind() == reflect.Pointer {
			return nil, fmt.Errorf(
				"xgin: anonymous pointer field %s is not supported",
				field.Name,
			)
		}
		if nestedType.Kind() != reflect.Struct {
			continue
		}
		nestedFields, err := b.collectShadowFields(nestedType, source, targetPath)
		if err != nil {
			return nil, err
		}
		fields = append(fields, nestedFields...)
	}
	return fields, nil
}

// checkFieldMapping checks whether a field supports the selected string mapping source.
func (b *Binder) checkFieldMapping(field reflect.StructField, source bindSource) error {
	var tag string
	switch source {
	case bindSourceBody, bindSourceFile:
		// Body decoders and the isolated upload mapper do not use string mapping.
		return nil
	case bindSourceURI:
		tag = "uri"
	case bindSourceHeader:
		tag = "header"
	case bindSourceQuery, bindSourceForm:
		tag = "form"
	default:
		return fmt.Errorf("xgin: unsupported mapping source %d", source)
	}
	if err := b.checkMappingType(field.Type, tag, make(map[reflect.Type]bool)); err != nil {
		return fmt.Errorf("xgin: field %s: %w", field.Name, err)
	}
	return nil
}

// ignoredEmbeddedField checks a parent ignore tag before exposing promoted fields.
func (b *Binder) ignoredEmbeddedField(field reflect.StructField, source bindSource) bool {
	switch source {
	case bindSourceURI:
		return field.Tag.Get("uri") == "-"
	case bindSourceHeader:
		return field.Tag.Get("header") == "-"
	case bindSourceQuery, bindSourceForm, bindSourceFile:
		return field.Tag.Get("form") == "-"
	case bindSourceBody:
		for _, tag := range [...]string{"json", "xml", "yaml", "toml", "codec", "bson"} {
			if field.Tag.Get(tag) == "-" {
				return true
			}
		}
	}
	return false
}

// checkMappingType rejects unsafe nested mappings and recursive pointer types.
func (b *Binder) checkMappingType(t reflect.Type, tag string, path map[reflect.Type]bool) error {
	pointers := make(map[reflect.Type]bool)
	for t.Kind() == reflect.Pointer {
		// A defined pointer type may refer to itself without reaching a struct,
		// so the struct recursion guard below cannot detect this cycle.
		if pointers[t] {
			return fmt.Errorf("recursive %s binding pointer type %s", tag, t)
		}
		pointers[t] = true
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	// Gin walks struct fields even when the parent parameter is absent. A cycle
	// would keep allocating temporary pointers instead of reaching a leaf.
	if path[t] {
		return fmt.Errorf("recursive %s binding type %s", tag, t)
	}

	path[t] = true
	defer delete(path, t)

	for field := range t.Fields() {
		if field.Tag.Get(tag) == "-" {
			continue
		}
		if field.PkgPath != "" {
			if field.Anonymous {
				return fmt.Errorf("unexported embedded field %s cannot be bound", field.Name)
			}
			continue
		}
		// Explicit child tags prevent Gin's Go-field-name fallback from reading
		// parameters belonging to another source. BindUnmarshaler structs follow
		// the same rule because Gin still recurses when their parent key is absent.
		if !field.Anonymous && !b.hasBindTag(field, tag) {
			return fmt.Errorf("nested field %s requires %s tag", field.Name, tag)
		}
		if err := b.checkMappingType(field.Type, tag, path); err != nil {
			return err
		}
	}

	return nil
}

// sourceOf determines a field's primary source and rejects conflicting tags.
func (b *Binder) sourceOf(field reflect.StructField) (bindSource, error) {
	if _, found := field.Tag.Lookup("query"); found {
		return 0, fmt.Errorf(
			"xgin: field %s uses unsupported query tag; use form instead",
			field.Name,
		)
	}

	if _, found := field.Tag.Lookup("protobuf"); found {
		return 0, fmt.Errorf(
			"xgin: field %s uses unsupported protobuf tag",
			field.Name,
		)
	}

	if _, found := field.Tag.Lookup("msgpack"); found {
		return 0, fmt.Errorf(
			"xgin: field %s uses unsupported msgpack tag; use codec instead",
			field.Name,
		)
	}

	// Body tags take precedence over form on writes. The query shadow still
	// includes these form fields for GET/HEAD; URI and header remain exclusive.
	sources := make([]bindSource, 0, 4)

	if b.hasBindTag(field, "uri") {
		sources = append(sources, bindSourceURI)
	}
	if b.hasBindTag(field, "header") {
		sources = append(sources, bindSourceHeader)
	}
	if b.hasBodyTag(field) {
		sources = append(sources, bindSourceBody)
	} else if b.hasBindTag(field, "form") {
		sources = append(sources, bindSourceForm)
	}

	if len(sources) <= 1 {
		if len(sources) == 0 {
			return 0, nil
		}
		source := sources[0]
		isFile, err := b.fileType(field.Type)
		if err != nil {
			return 0, fmt.Errorf("xgin: field %s: %w", field.Name, err)
		}
		if isFile {
			if source != bindSourceForm {
				return 0, fmt.Errorf("xgin: file field %s requires an exclusive form tag", field.Name)
			}
			return bindSourceFile, nil
		}
		if err = b.checkNestedFiles(field.Type, make(map[reflect.Type]bool)); err != nil {
			return 0, fmt.Errorf("xgin: field %s: %w", field.Name, err)
		}
		return source, nil
	}
	return 0, fmt.Errorf(
		"xgin: field %s declares more than one bind source",
		field.Name,
	)
}

// fileType recognizes standard multipart headers and their direct slices or arrays.
func (b *Binder) fileType(t reflect.Type) (bool, error) {
	if t == fileHeaderType || t == reflect.PointerTo(fileHeaderType) {
		return true, nil
	}
	if (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) && t.Name() == "" {
		if t.Elem() == fileHeaderType || t.Elem() == reflect.PointerTo(fileHeaderType) {
			return true, nil
		}
	}

	// Defined lookalikes and extra wrappers must not fall through to a string or
	// body decoder, which could accept client-supplied FileHeader metadata.
	seen := make(map[reflect.Type]bool)
	base := t
	for base.Kind() == reflect.Pointer || base.Kind() == reflect.Slice || base.Kind() == reflect.Array {
		if seen[base] {
			return false, nil
		}
		seen[base] = true
		base = base.Elem()
	}
	if base.Kind() == reflect.Struct && base.ConvertibleTo(fileHeaderType) {
		return false, fmt.Errorf("unsupported multipart file type %s; use standard FileHeader types", t)
	}
	return false, nil
}

// checkNestedFiles keeps uploads out of named nested string and body mappings.
func (b *Binder) checkNestedFiles(t reflect.Type, seen map[reflect.Type]bool) error {
	if seen[t] {
		return nil
	}
	seen[t] = true
	isFile, err := b.fileType(t)
	if err != nil {
		return err
	}
	if isFile {
		return errors.New("multipart file fields must be top-level or anonymously embedded")
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return b.checkNestedFiles(t.Elem(), seen)
	case reflect.Map:
		if err := b.checkNestedFiles(t.Key(), seen); err != nil {
			return err
		}
		return b.checkNestedFiles(t.Elem(), seen)
	case reflect.Struct:
		for field := range t.Fields() {
			if field.PkgPath != "" {
				continue
			}
			if err := b.checkNestedFiles(field.Type, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasBindTag reports an explicitly declared tag other than the exact ignore marker "-".
func (b *Binder) hasBindTag(field reflect.StructField, tag string) bool {
	value, found := field.Tag.Lookup(tag)
	return found && value != "-"
}

// hasBodyTag reports an encoding tag supported by shadow body binding.
func (b *Binder) hasBodyTag(field reflect.StructField) bool {
	// Protobuf requires a complete proto.Message and cannot use a shadow struct.
	// Gin's MessagePack decoder reads codec tags, not msgpack tags.
	return b.hasBindTag(field, "json") ||
		b.hasBindTag(field, "xml") ||
		b.hasBindTag(field, "yaml") ||
		b.hasBindTag(field, "toml") ||
		b.hasBindTag(field, "codec") ||
		b.hasBindTag(field, "bson")
}

// bodyFormatsOf intersects the encodings explicitly declared by every body field.
func (b *Binder) bodyFormatsOf(plan shadowPlan) bodyFormat {
	if !plan.enabled() {
		return 0
	}
	// Requiring explicit tags for every field prevents fallback decoding by Go
	// field name, which could silently accept a format the DTO did not declare.
	formats := allBodyFormats
	hasMsgpackBinding := binding.Default(http.MethodPost, "application/msgpack").Name() == "msgpack"

	for field := range plan.typeOf.Fields() {
		var fieldFormats bodyFormat
		if b.hasBindTag(field, "json") {
			fieldFormats |= bodyFormatJSON
		}
		if b.hasBindTag(field, "xml") {
			fieldFormats |= bodyFormatXML
		}
		if b.hasBindTag(field, "yaml") {
			fieldFormats |= bodyFormatYAML
		}
		if b.hasBindTag(field, "toml") {
			fieldFormats |= bodyFormatTOML
		}
		if b.hasBindTag(field, "codec") && hasMsgpackBinding {
			fieldFormats |= bodyFormatMsgpack
		}
		if b.hasBindTag(field, "bson") {
			fieldFormats |= bodyFormatBSON
		}
		if b.hasBindTag(field, "form") {
			fieldFormats |= bodyFormatForm
		}
		formats &= fieldFormats
	}
	return formats
}

// ---------------------------------------------------------------------------------------------------------------------

// bindingPlan holds immutable shadows for one DTO. Empty shadows are skipped.
type bindingPlan struct {
	uri         shadowPlan
	header      shadowPlan
	query       shadowPlan
	form        shadowPlan
	file        shadowPlan
	body        shadowPlan
	bodyFormats bodyFormat
}

// bind executes source-isolated binding and copies successful results into target.
func (p *bindingPlan) bind(c *gin.Context, target reflect.Value, contentType string) error {
	if (p.query.enabled() || p.form.enabled()) &&
		c.Request.URL.RawQuery != "" {
		if _, err := url.ParseQuery(c.Request.URL.RawQuery); err != nil {
			return fmt.Errorf("xgin: parse query: %w", err)
		}
	}

	if err := p.uri.bind(c, target, contentType); err != nil {
		return fmt.Errorf("xgin: bind uri: %w", err)
	}

	if err := p.header.bind(c, target, contentType); err != nil {
		return fmt.Errorf("xgin: bind header: %w", err)
	}

	// Files are validated even on read-only and non-multipart requests. Their
	// shadow remains zero-valued unless real multipart file parts were parsed.
	if err := p.file.bind(c, target, contentType); err != nil {
		return fmt.Errorf("xgin: bind files: %w", err)
	}

	isReadOnly := c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead
	if isReadOnly {
		if err := p.query.bind(c, target, contentType); err != nil {
			return fmt.Errorf("xgin: bind query: %w", err)
		}
		return nil
	}

	if err := p.form.bind(c, target, contentType); err != nil {
		return fmt.Errorf("xgin: bind form: %w", err)
	}

	if err := p.body.bind(c, target, contentType); err != nil {
		return fmt.Errorf("xgin: bind body: %w", err)
	}

	return nil
}

// contentType validates relevant write requests and returns their normalized media type.
func (p *bindingPlan) contentType(c *gin.Context) (string, error) {
	isReadOnly := c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead
	if isReadOnly || (!p.body.enabled() && !p.form.enabled() && !p.file.enabled()) {
		return "", nil
	}

	values := c.Request.Header.Values("Content-Type")
	if len(values) > 1 {
		// Content-Type is a singleton header. Choosing the first value would let
		// different HTTP components interpret the same request using different codecs.
		return "", ErrUnsupportedContentType
	}
	if len(values) == 0 || values[0] == "" {
		if p.body.enabled() {
			return "", ErrMissingContentType
		}
		return "", nil
	}

	contentType, _, err := mime.ParseMediaType(values[0])
	if err != nil {
		if p.body.enabled() {
			return "", ErrUnsupportedContentType
		}
		// Form-only DTOs keep their existing query fallback for invalid media types.
		return "", nil
	}
	if p.body.enabled() && !p.isSupportedBodyContentType(contentType) {
		return "", ErrUnsupportedContentType
	}
	return contentType, nil
}

// isSupportedBodyContentType checks whether all body fields support contentType.
func (p *bindingPlan) isSupportedBodyContentType(contentType string) bool {
	var format bodyFormat

	switch contentType {
	case binding.MIMEJSON:
		format = bodyFormatJSON
	case binding.MIMEXML, binding.MIMEXML2:
		format = bodyFormatXML
	case binding.MIMEYAML, binding.MIMEYAML2:
		format = bodyFormatYAML
	case binding.MIMETOML:
		format = bodyFormatTOML
	case "application/x-msgpack", "application/msgpack":
		format = bodyFormatMsgpack
	case binding.MIMEBSON:
		format = bodyFormatBSON
	case binding.MIMEPOSTForm, binding.MIMEMultipartPOSTForm:
		format = bodyFormatForm
	default:
		return false
	}
	return p.bodyFormats&format != 0
}

// ---------------------------------------------------------------------------------------------------------------------

// shadowPlan holds a source-specific DTO type and its original field mappings.
type shadowPlan struct {
	typeOf   reflect.Type
	mappings []fieldMapping
	source   bindSource
}

// enabled reports whether the shadow contains any fields.
func (p shadowPlan) enabled() bool {
	return p.typeOf != nil
}

// bind validates a fresh shadow before copying any fields into target.
func (p shadowPlan) bind(c *gin.Context, target reflect.Value, contentType string) error {
	if !p.enabled() {
		return nil
	}

	shadow := reflect.New(p.typeOf)

	if err := p.bindShadow(c, shadow.Interface(), contentType); err != nil {
		return err
	}

	if p.source == bindSourceBody && contentType == binding.MIMEBSON && binding.Validator != nil {
		// Gin's BSON decoder omits the binding validation performed by other decoders.
		if err := binding.Validator.ValidateStruct(shadow.Interface()); err != nil {
			return err
		}
	}

	// Mappings were compiled alongside the dynamic fields and include embedded paths.
	for _, mapping := range p.mappings {
		target.FieldByIndex(mapping.targetPath).Set(shadow.Elem().Field(mapping.shadowIndex))
	}

	return nil
}

// bindShadow selects Gin's native binder for the source and normalized media type.
func (p shadowPlan) bindShadow(c *gin.Context, shadow any, contentType string) error {
	switch p.source {
	case bindSourceURI:
		return c.ShouldBindUri(shadow)
	case bindSourceHeader:
		return c.ShouldBindHeader(shadow)
	case bindSourceQuery:
		return c.ShouldBindQuery(shadow)
	case bindSourceForm:
		return p.bindForm(c, shadow, contentType)
	case bindSourceFile:
		if err := p.bindFiles(c, reflect.ValueOf(shadow).Elem(), contentType); err != nil {
			return err
		}
		if binding.Validator != nil {
			return binding.Validator.ValidateStruct(shadow)
		}
		return nil
	case bindSourceBody:
		// Select with the normalized type while leaving the original header intact.
		// Multipart parsing still needs its boundary parameter from that header.
		return c.ShouldBindWith(shadow, binding.Default(c.Request.Method, contentType))
	default:
		return errors.New("xgin: unknown bind source")
	}
}

// bindForm preserves Gin's query and form merging rules.
func (p shadowPlan) bindForm(c *gin.Context, shadow any, contentType string) error {
	isReadOnly := c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead
	if isReadOnly || !p.isFormRequest(contentType) {
		return c.ShouldBindQuery(shadow)
	}
	return c.ShouldBindWith(shadow, binding.Default(c.Request.Method, contentType))
}

// bindFiles reads only multipart file parts, never form values or query metadata.
func (p shadowPlan) bindFiles(c *gin.Context, shadow reflect.Value, contentType string) error {
	isReadOnly := c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead
	if isReadOnly || contentType != binding.MIMEMultipartPOSTForm {
		return nil
	}
	form, err := c.MultipartForm()
	if err != nil {
		return err
	}
	for index := 0; index < p.typeOf.NumField(); index++ {
		field := p.typeOf.Field(index)
		key, _, _ := strings.Cut(field.Tag.Get("form"), ",")
		if key == "" {
			key = field.Name
		}
		files := form.File[key]
		if len(files) == 0 {
			continue
		}
		if err := p.setFiles(shadow.Field(index), files); err != nil {
			return fmt.Errorf("file field %s: %w", field.Name, err)
		}
	}
	return nil
}

// setFiles applies Gin's standard header shapes and exact array-length rule.
func (p shadowPlan) setFiles(field reflect.Value, files []*multipart.FileHeader) error {
	if len(files) == 0 || files[0] == nil {
		return binding.ErrMultiFileHeader
	}
	switch field.Kind() {
	case reflect.Pointer:
		field.Set(reflect.ValueOf(files[0]))
	case reflect.Struct:
		field.Set(reflect.ValueOf(*files[0]))
	case reflect.Slice, reflect.Array:
		if field.Kind() == reflect.Slice {
			field.Set(reflect.MakeSlice(field.Type(), len(files), len(files)))
		} else if field.Len() != len(files) {
			return binding.ErrMultiFileHeaderLenInvalid
		}
		for index := range files {
			if err := p.setFiles(field.Index(index), files[index:index+1]); err != nil {
				return err
			}
		}
	default:
		return binding.ErrMultiFileHeader
	}
	return nil
}

// isFormRequest reports a normalized form media type.
func (p shadowPlan) isFormRequest(contentType string) bool {
	return contentType == binding.MIMEPOSTForm || contentType == binding.MIMEMultipartPOSTForm
}

// ---------------------------------------------------------------------------------------------------------------------
