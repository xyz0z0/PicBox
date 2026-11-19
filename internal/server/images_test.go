package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"github.com/google/uuid"
)

// ---- helpers for the images worker tests ----
// 前缀保持唯一，避免与 core/web 同包测试辅助函数重名。

type imgTestEnv struct {
	app     *App
	handler http.Handler
}

type imgTestAuth struct {
	cookie *http.Cookie
	bearer string
}

func imgTestNewApp(t *testing.T, cfg Config) *imgTestEnv {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New() 失败: %v", err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Errorf("Close() 失败: %v", err)
		}
	})
	return &imgTestEnv{app: app, handler: app.Handler()}
}

func (e *imgTestEnv) do(t *testing.T, method, target string, body io.Reader, contentType string, auth imgTestAuth) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if auth.cookie != nil {
		request.AddCookie(auth.cookie)
	}
	if auth.bearer != "" {
		request.Header.Set("Authorization", "Bearer "+auth.bearer)
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, request)
	return recorder
}

func imgTestCookieAuth(cookie *http.Cookie) imgTestAuth { return imgTestAuth{cookie: cookie} }
func imgTestBearerAuth(raw string) imgTestAuth          { return imgTestAuth{bearer: raw} }

func imgTestJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON (status=%d): %v: %s", recorder.Code, err, recorder.Body.String())
	}
	return body
}

func imgTestErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	body := imgTestJSON(t, recorder)
	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error 对象: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	code, _ := errorBody["code"].(string)
	return code
}

func (e *imgTestEnv) registerAdmin(t *testing.T, email, password string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		t.Fatalf("序列化注册请求失败: %v", err)
	}
	recorder := e.do(t, http.MethodPost, "/api/auth/register", bytes.NewReader(payload), "application/json", imgTestAuth{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("注册失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == authCookieName {
			return cookie
		}
	}
	t.Fatalf("注册响应没有 %s Cookie", authCookieName)
	return nil
}

func (e *imgTestEnv) addUser(t *testing.T, email string) (int64, *http.Cookie) {
	t.Helper()
	result, err := e.app.db.Exec(`INSERT INTO "User" (email, username, passwordHash, role, sessionVersion, mustChangePassword, createdAt)
		VALUES (?, ?, 'not-used-in-tests', 'USER', 0, 0, ?)`,
		email, email, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("插入测试用户失败: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("读取测试用户 id 失败: %v", err)
	}
	token, err := e.app.signToken(id, 0)
	if err != nil {
		t.Fatalf("签发测试用户令牌失败: %v", err)
	}
	return id, &http.Cookie{Name: authCookieName, Value: token}
}

func (e *imgTestEnv) addAPIKey(t *testing.T, userID int64) string {
	t.Helper()
	raw := "img-test-key-" + uuid.NewString()
	if _, err := e.app.db.Exec(`INSERT INTO "ApiKey" (id, userId, name, keyHash, prefix, createdAt)
		VALUES (?, ?, 'test', ?, 'img', ?)`,
		uuid.NewString(), userID, hashAPIKey(raw), time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("插入测试 API Key 失败: %v", err)
	}
	return raw
}

func (e *imgTestEnv) setQuota(t *testing.T, userID int64, quota int64) {
	t.Helper()
	if _, err := e.app.db.Exec(`UPDATE "User" SET quotaBytes = ? WHERE id = ?`, quota, userID); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
}

type imgTestFormField struct {
	Name  string
	Value string
}

type imgTestUploadOptions struct {
	FileName   string
	Data       []byte
	Fields     []imgTestFormField
	ExtraFiles []imgTestFormField
}

func imgTestMultipart(t *testing.T, opts imgTestUploadOptions) (*bytes.Buffer, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for _, field := range opts.Fields {
		if err := writer.WriteField(field.Name, field.Value); err != nil {
			t.Fatalf("写入表单字段 %s 失败: %v", field.Name, err)
		}
	}
	if opts.FileName != "" {
		part, err := writer.CreateFormFile("file", opts.FileName)
		if err != nil {
			t.Fatalf("创建文件部件失败: %v", err)
		}
		if _, err := part.Write(opts.Data); err != nil {
			t.Fatalf("写入文件部件失败: %v", err)
		}
	}
	for _, extra := range opts.ExtraFiles {
		part, err := writer.CreateFormFile(extra.Name, extra.Value)
		if err != nil {
			t.Fatalf("创建额外文件部件失败: %v", err)
		}
		if _, err := part.Write(opts.Data); err != nil {
			t.Fatalf("写入额外文件部件失败: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("结束 multipart 失败: %v", err)
	}
	return &buffer, writer.FormDataContentType()
}

func (e *imgTestEnv) upload(t *testing.T, auth imgTestAuth, opts imgTestUploadOptions) *httptest.ResponseRecorder {
	t.Helper()
	body, contentType := imgTestMultipart(t, opts)
	return e.do(t, http.MethodPost, "/api/images", body, contentType, auth)
}

func imgTestTagsJSON(t *testing.T, tags []string) string {
	t.Helper()
	encoded, err := json.Marshal(tags)
	if err != nil {
		t.Fatalf("序列化标签失败: %v", err)
	}
	return string(encoded)
}

// imgTestUploadBytes uploads raw bytes and returns the decoded response body.
func (e *imgTestEnv) imgTestUploadBytes(t *testing.T, auth imgTestAuth, fileName string, data []byte, tags []string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	fields := make([]imgTestFormField, 0, 1)
	if tags != nil {
		fields = append(fields, imgTestFormField{Name: "tags", Value: imgTestTagsJSON(t, tags)})
	}
	recorder := e.upload(t, auth, imgTestUploadOptions{FileName: fileName, Data: data, Fields: fields})
	if recorder.Code != http.StatusOK {
		return recorder, nil
	}
	return recorder, imgTestJSON(t, recorder)
}

func imgTestMustImageID(t *testing.T, body map[string]any) string {
	t.Helper()
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("上传响应缺少 id: %v", body)
	}
	return id
}

func imgTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.SetRGBA(x, y, color.RGBA{uint8(x % 251), uint8(y % 241), uint8((x + y) % 239), 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, canvas); err != nil {
		t.Fatalf("生成 PNG fixture 失败: %v", err)
	}
	return buffer.Bytes()
}

func imgTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.SetRGBA(x, y, color.RGBA{uint8(x % 255), uint8(y % 255), 64, 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("生成 JPEG fixture 失败: %v", err)
	}
	return buffer.Bytes()
}

func imgTestGIF(t *testing.T, width, height, frames int) []byte {
	t.Helper()
	palette := color.Palette{
		color.RGBA{0, 0, 0, 255},
		color.RGBA{255, 255, 255, 255},
		color.RGBA{220, 40, 40, 255},
	}
	animation := &gif.GIF{LoopCount: 0}
	for frame := 0; frame < frames; frame++ {
		canvas := image.NewPaletted(image.Rect(0, 0, width, height), palette)
		for index := range canvas.Pix {
			canvas.Pix[index] = uint8((index + frame) % len(palette))
		}
		animation.Image = append(animation.Image, canvas)
		animation.Delay = append(animation.Delay, 8)
	}
	var buffer bytes.Buffer
	if err := gif.EncodeAll(&buffer, animation); err != nil {
		t.Fatalf("生成 GIF fixture 失败: %v", err)
	}
	return buffer.Bytes()
}

func imgTestAnimatedWebP(t *testing.T, width, height, frames int) []byte {
	t.Helper()
	animation := &nativewebp.Animation{LoopCount: 0}
	for frame := 0; frame < frames; frame++ {
		canvas := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				canvas.SetRGBA(x, y, color.RGBA{uint8((x + frame) % 255), uint8(y % 255), 128, 255})
			}
		}
		animation.Images = append(animation.Images, canvas)
		animation.Durations = append(animation.Durations, 50)
		animation.Disposals = append(animation.Disposals, 0)
	}
	var buffer bytes.Buffer
	if err := nativewebp.EncodeAll(&buffer, animation, nil); err != nil {
		t.Fatalf("生成动画 WebP fixture 失败: %v", err)
	}
	return buffer.Bytes()
}

// imgTestAnimatedWebPWithCanvas 用 nativewebp 生成首帧矩形为 rect 的动画：画布取
// rect.Max，帧偏移按规范写成 rect.Min/2。
func imgTestAnimatedWebPWithCanvas(t *testing.T, rect image.Rectangle, frames int) []byte {
	t.Helper()
	animation := &nativewebp.Animation{LoopCount: 0}
	for frame := 0; frame < frames; frame++ {
		canvas := image.NewRGBA(rect)
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			for x := rect.Min.X; x < rect.Max.X; x++ {
				canvas.SetRGBA(x, y, color.RGBA{uint8((x + frame) % 255), uint8(y % 255), 128, 255})
			}
		}
		animation.Images = append(animation.Images, canvas)
		animation.Durations = append(animation.Durations, 50)
		animation.Disposals = append(animation.Disposals, 0)
	}
	var buffer bytes.Buffer
	if err := nativewebp.EncodeAll(&buffer, animation, nil); err != nil {
		t.Fatalf("生成偏移动画 WebP fixture 失败: %v", err)
	}
	return buffer.Bytes()
}

// imgTestSolidImage 生成纯色图像，用于断言首帧像素。
func imgTestSolidImage(rect image.Rectangle, fill color.NRGBA) image.Image {
	canvas := image.NewNRGBA(rect)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			canvas.SetNRGBA(x, y, fill)
		}
	}
	return canvas
}

// imgTestWebPChunk 组装一个带 RIFF 补齐字节的 chunk。
func imgTestWebPChunk(id string, data []byte) []byte {
	if len(id) != 4 {
		panic("webp chunk id must be four bytes")
	}
	var buffer bytes.Buffer
	buffer.WriteString(id)
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(data)))
	buffer.Write(size[:])
	buffer.Write(data)
	if len(data)%2 == 1 {
		buffer.WriteByte(0)
	}
	return buffer.Bytes()
}

// imgTestWebPFile 把 chunk 拼成 RIFF/WEBP 文件。
func imgTestWebPFile(chunks ...[]byte) []byte {
	body := bytes.Join(chunks, nil)
	var buffer bytes.Buffer
	buffer.WriteString("RIFF")
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(4+len(body)))
	buffer.Write(size[:])
	buffer.WriteString("WEBP")
	buffer.Write(body)
	return buffer.Bytes()
}

// imgTestWebPAnimatedHeader 生成 'VP8X' + 'ANIM' 两个 chunk。
func imgTestWebPAnimatedHeader(canvasWidth, canvasHeight int, flags byte, background uint32, loopCount uint16) []byte {
	vp8x := make([]byte, 10)
	vp8x[0] = flags
	put := func(b []byte, value int) {
		b[0], b[1], b[2] = byte(value), byte(value>>8), byte(value>>16)
	}
	put(vp8x[4:7], canvasWidth-1)
	put(vp8x[7:10], canvasHeight-1)
	anim := make([]byte, 6)
	binary.LittleEndian.PutUint32(anim[0:4], background)
	binary.LittleEndian.PutUint16(anim[4:6], loopCount)
	return bytes.Join([][]byte{imgTestWebPChunk("VP8X", vp8x), imgTestWebPChunk("ANIM", anim)}, nil)
}

// imgTestWebPAnmfHeader 生成 'ANMF' 载荷的 16 字节帧参数。
func imgTestWebPAnmfHeader(x, y, width, height int, flags byte) []byte {
	if x%2 != 0 || y%2 != 0 {
		panic("webp frame offsets are stored divided by two")
	}
	header := make([]byte, 16)
	put := func(b []byte, value int) {
		b[0], b[1], b[2] = byte(value), byte(value>>8), byte(value>>16)
	}
	put(header[0:3], x/2)
	put(header[3:6], y/2)
	put(header[6:9], width-1)
	put(header[9:12], height-1)
	put(header[12:15], 100)
	header[15] = flags
	return header
}

// imgTestWebPAnmf 生成完整的 'ANMF' 载荷（帧参数 + 已补齐的子块）。
func imgTestWebPAnmf(x, y, width, height int, flags byte, chunks ...[]byte) []byte {
	return append(imgTestWebPAnmfHeader(x, y, width, height, flags), bytes.Join(chunks, nil)...)
}

// imgTestWebPChunks 按 FourCC 索引静态 WebP 的顶层 chunk。
func imgTestWebPChunks(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		t.Fatalf("不是 WebP 容器: %d 字节", len(data))
	}
	chunks := make(map[string][]byte, 4)
	offset := 12
	for offset+8 <= len(data) {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if size > len(data)-offset {
			t.Fatalf("chunk %q 长度越界: %d > %d", id, size, len(data)-offset)
		}
		chunks[id] = data[offset : offset+size]
		offset += size
		if size%2 == 1 {
			offset++
		}
	}
	return chunks
}

// imgTestWebPFrameChunks 把图像编码成静态 WebP，再取出可直接放进 ANMF 的子块。
func imgTestWebPFrameChunks(t *testing.T, source image.Image) [][]byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := nativewebp.Encode(&buffer, source, nil); err != nil {
		t.Fatalf("生成静态帧失败: %v", err)
	}
	chunks := imgTestWebPChunks(t, buffer.Bytes())
	encoded := make([][]byte, 0, 2)
	for _, id := range []string{"ALPH", "VP8 ", "VP8L"} {
		if data, ok := chunks[id]; ok {
			encoded = append(encoded, imgTestWebPChunk(id, data))
		}
	}
	return encoded
}

// imgTestExpectPixel 断言像素的 8 位 NRGBA 完全一致（无损编码与透明区域可以精确比较）。
func imgTestExpectPixel(t *testing.T, img image.Image, x, y int, want color.NRGBA) {
	t.Helper()
	got := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
	if got != want {
		t.Fatalf("像素 (%d,%d) = %v，期望 %v", x, y, got, want)
	}
}

// imgTestExpectPixelNear 只对颜色做容差比较（有损 VP8），alpha 必须精确。
func imgTestExpectPixelNear(t *testing.T, img image.Image, x, y int, want color.NRGBA, tolerance int) {
	t.Helper()
	got := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
	diff := func(v, want uint8) int {
		if v > want {
			return int(v - want)
		}
		return int(want - v)
	}
	if diff(got.R, want.R) > tolerance || diff(got.G, want.G) > tolerance || diff(got.B, want.B) > tolerance || got.A != want.A {
		t.Fatalf("像素 (%d,%d) = %v，期望 ≈%v (容差 %d)", x, y, got, want, tolerance)
	}
}

// imgTestLossyAlphaWebPBase64 是 8x8 的合成样本：左半 4 列不透明红，右半透明，
// 由 PIL 生成 RGBA PNG 后用 `cwebp -q 80 -alpha_q 100 -exact` 编码，用于覆盖
// 「有损 VP8 + ALPH 帧」路径。不要用真实上传数据替换它。
const imgTestLossyAlphaWebPBase64 = "UklGRm4AAABXRUJQVlA4WAoAAAAQAAAABwAABwAAQUxQSAsAAAABDzD/ERGiDxH9DwBWUDggPAAAAHACAJ0BKggACAABQCYlqAJ0cgCr/9wABN2HI/AA/vaT/3KkP+aoTdHsyWsP/9h18sDTMX/86D5YGl0AAA=="

func imgTestWebP(t *testing.T, width, height int) []byte {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			canvas.SetRGBA(x, y, color.RGBA{32, uint8(x % 255), uint8(y % 255), 255})
		}
	}
	var buffer bytes.Buffer
	if err := nativewebp.Encode(&buffer, canvas, nil); err != nil {
		t.Fatalf("生成 WebP fixture 失败: %v", err)
	}
	return buffer.Bytes()
}

// imgTestPNGHeaderOnly builds a PNG signature + IHDR with arbitrary dimensions and
// a valid CRC. DecodeConfig accepts it, so the pixel guard is exercised without
// allocating a huge image.
func imgTestPNGHeaderOnly(width, height uint32) []byte {
	var buffer bytes.Buffer
	buffer.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // truecolour
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(ihdr)))
	buffer.Write(length)
	chunkTypeAndData := append([]byte("IHDR"), ihdr...)
	buffer.Write(chunkTypeAndData)
	checksum := make([]byte, 4)
	binary.BigEndian.PutUint32(checksum, crc32.ChecksumIEEE(chunkTypeAndData))
	buffer.Write(checksum)
	return buffer.Bytes()
}

func imgTestImagesDirEntries(t *testing.T, dataDir string) []string {
	t.Helper()
	names := make([]string, 0, 8)
	root := filepath.Join(dataDir, "images")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.IsDir() {
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			names = append(names, relative)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	sort.Strings(names)
	return names
}

func imgTestImageCount(t *testing.T, env *imgTestEnv) int {
	t.Helper()
	var count int
	if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "Image"`).Scan(&count); err != nil {
		t.Fatalf("统计图片失败: %v", err)
	}
	return count
}

func imgTestListIDs(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()
	body := imgTestJSON(t, recorder)
	rawItems, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items 不是数组: %s", recorder.Body.String())
	}
	ids := make([]string, 0, len(rawItems))
	for _, rawItem := range rawItems {
		item, ok := rawItem.(map[string]any)
		if !ok {
			t.Fatalf("item 不是对象: %v", rawItem)
		}
		id, _ := item["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func imgTestSetHidden(t *testing.T, env *imgTestEnv, auth imgTestAuth, imageID string, visible bool) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"visible": visible})
	if err != nil {
		t.Fatalf("序列化 PATCH 请求失败: %v", err)
	}
	recorder := env.do(t, http.MethodPatch, "/api/images/"+imageID, bytes.NewReader(payload), "application/json", auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PATCH visible=%v 失败: status=%d body=%s", visible, recorder.Code, recorder.Body.String())
	}
}

func imgTestTrash(t *testing.T, env *imgTestEnv, auth imgTestAuth, imageID string) {
	t.Helper()
	recorder := env.do(t, http.MethodDelete, "/api/images/"+imageID, nil, "", auth)
	if recorder.Code != http.StatusOK {
		t.Fatalf("软删除失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// ---- upload ----

func TestImagesUploadPreservesOriginalBytesAndBuildsThumbnail(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "uploader@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	cases := []struct {
		name        string
		fileName    string
		data        []byte
		contentType string
		width       int
		height      int
	}{
		{name: "png", fileName: "sample.png", data: imgTestPNG(t, 800, 600), contentType: "image/png", width: 800, height: 600},
		{name: "jpeg", fileName: "sample.jpg", data: imgTestJPEG(t, 640, 480), contentType: "image/jpeg", width: 640, height: 480},
		{name: "gif", fileName: "sample.gif", data: imgTestGIF(t, 120, 90, 3), contentType: "image/gif", width: 120, height: 90},
		{name: "webp", fileName: "sample.webp", data: imgTestWebP(t, 500, 250), contentType: "image/webp", width: 500, height: 250},
		{name: "animated-webp", fileName: "animated.webp", data: imgTestAnimatedWebP(t, 64, 48, 3), contentType: "image/webp", width: 64, height: 48},
		// 画布 40x28，首帧 32x24 偏移 (8,4)：数据库宽高取画布，不取帧尺寸。
		{name: "animated-webp-offset", fileName: "animated-offset.webp", data: imgTestAnimatedWebPWithCanvas(t, image.Rect(8, 4, 40, 28), 2), contentType: "image/webp", width: 40, height: 28},
		{name: "small-png", fileName: "small.png", data: imgTestPNG(t, 20, 10), contentType: "image/png", width: 20, height: 10},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder, body := env.imgTestUploadBytes(t, auth, testCase.fileName, testCase.data, []string{"beta", "alpha"})
			if recorder.Code != http.StatusOK {
				t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			imageID := imgTestMustImageID(t, body)
			if body["ok"] != true {
				t.Fatalf("上传响应 ok 不为 true: %v", body)
			}
			if got := body["tag"]; got != "alpha" {
				t.Fatalf("tag 应为排序后的第一个标签 alpha，实际 %v", got)
			}
			tags, _ := body["tags"].([]any)
			if len(tags) != 2 || tags[0] != "alpha" || tags[1] != "beta" {
				t.Fatalf("tags 不符合预期: %v", body["tags"])
			}

			// 原图字节必须逐字节保留（含 GIF 动画字节）。
			original := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=original", nil, "", imgTestAuth{})
			if original.Code != http.StatusOK {
				t.Fatalf("读取原图失败: status=%d body=%s", original.Code, original.Body.String())
			}
			if !bytes.Equal(original.Body.Bytes(), testCase.data) {
				t.Fatalf("原图字节被改写: 期望 %d 字节，实际 %d 字节", len(testCase.data), original.Body.Len())
			}
			if contentType := original.Header().Get("Content-Type"); contentType != testCase.contentType {
				t.Fatalf("原图 Content-Type = %q，期望 %q", contentType, testCase.contentType)
			}

			// 缩略图是 WebP，取首帧，且不超过 400x400、不放大。
			thumb := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=thumb", nil, "", imgTestAuth{})
			if thumb.Code != http.StatusOK {
				t.Fatalf("读取缩略图失败: status=%d body=%s", thumb.Code, thumb.Body.String())
			}
			if contentType := thumb.Header().Get("Content-Type"); contentType != "image/webp" {
				t.Fatalf("缩略图 Content-Type = %q，期望 image/webp", contentType)
			}
			decoded, format, err := image.Decode(bytes.NewReader(thumb.Body.Bytes()))
			if err != nil {
				t.Fatalf("缩略图无法解码: %v", err)
			}
			if format != "webp" {
				t.Fatalf("缩略图格式 = %q，期望 webp", format)
			}
			width, height := decoded.Bounds().Dx(), decoded.Bounds().Dy()
			if width > 400 || height > 400 {
				t.Fatalf("缩略图超过 400x400: %dx%d", width, height)
			}
			expectedWidth, expectedHeight := testCase.width, testCase.height
			if expectedWidth > 400 || expectedHeight > 400 {
				scale := 400.0 / float64(max(expectedWidth, expectedHeight))
				expectedWidth = int(float64(expectedWidth) * scale)
				expectedHeight = int(float64(expectedHeight) * scale)
			}
			if abs(width-expectedWidth) > 1 || abs(height-expectedHeight) > 1 {
				t.Fatalf("缩略图尺寸 = %dx%d，期望约 %dx%d", width, height, expectedWidth, expectedHeight)
			}
			if testCase.width <= 400 && testCase.height <= 400 && (width != testCase.width || height != testCase.height) {
				t.Fatalf("小图缩略图被放大: %dx%d -> %dx%d", testCase.width, testCase.height, width, height)
			}

			// 数据库记录 sha256/size/mime 必须与原图一致。
			var storedSHA, storedMime, storedOriginalPath, storedThumbPath string
			var storedSize int64
			if err := env.app.db.QueryRow(`SELECT sha256, size, originalMimeType, originalPath, thumbPath FROM "Image" WHERE id = ?`, imageID).
				Scan(&storedSHA, &storedSize, &storedMime, &storedOriginalPath, &storedThumbPath); err != nil {
				t.Fatalf("读取图片记录失败: %v", err)
			}
			sum := sha256.Sum256(testCase.data)
			if storedSHA != hex.EncodeToString(sum[:]) {
				t.Fatalf("sha256 不匹配: %s", storedSHA)
			}
			if storedSize != int64(len(testCase.data)) {
				t.Fatalf("size = %d，期望 %d", storedSize, len(testCase.data))
			}
			if storedMime != testCase.contentType {
				t.Fatalf("originalMimeType = %q，期望 %q", storedMime, testCase.contentType)
			}
			for _, relative := range []string{storedOriginalPath, storedThumbPath} {
				absolute, err := env.app.imagesSafePath(relative)
				if err != nil {
					t.Fatalf("存储路径不安全 %q: %v", relative, err)
				}
				if _, err := os.Stat(absolute); err != nil {
					t.Fatalf("存储文件不存在 %q: %v", relative, err)
				}
			}
			// 暂存目录必须清空。
			entries, err := os.ReadDir(filepath.Join(env.app.cfg.DataDir, imagesTempDirName))
			if err != nil && !os.IsNotExist(err) {
				t.Fatalf("读取暂存目录失败: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("暂存目录残留文件: %v", entries)
			}
		})
	}
}

func TestImagesAnimatedWebPFirstFrameIsCompositedOnCanvas(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "animated@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	// 画布 20x16；首帧半透明红 8x8 在 (8,4)，第二帧蓝 8x8 在左上角。缩略图必须只反映首帧，
	// 且偏移按规范的 (2 * Frame X, 2 * Frame Y) 落位。
	animated := imgTestWebPFile(
		imgTestWebPAnimatedHeader(20, 16, 0x12, 0xFF00FF00, 0),
		imgTestWebPChunk("ANMF", imgTestWebPAnmf(8, 4, 8, 8, 0,
			imgTestWebPFrameChunks(t, imgTestSolidImage(image.Rect(0, 0, 8, 8), color.NRGBA{255, 0, 0, 128}))...)),
		imgTestWebPChunk("ANMF", imgTestWebPAnmf(0, 0, 8, 8, 0,
			imgTestWebPFrameChunks(t, imgTestSolidImage(image.Rect(0, 0, 8, 8), color.NRGBA{0, 0, 255, 255}))...)),
	)

	recorder, body := env.imgTestUploadBytes(t, auth, "animated.webp", animated, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imageID := imgTestMustImageID(t, body)

	// 原图字节逐字节保留。
	original := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=original", nil, "", imgTestAuth{})
	if original.Code != http.StatusOK {
		t.Fatalf("读取原图失败: status=%d body=%s", original.Code, original.Body.String())
	}
	if !bytes.Equal(original.Body.Bytes(), animated) {
		t.Fatalf("动画 WebP 原图字节被改写: 期望 %d 字节，实际 %d 字节", len(animated), original.Body.Len())
	}

	// 数据库元数据是画布尺寸，不是首帧尺寸。
	meta := env.do(t, http.MethodGet, "/api/images/"+imageID+"/meta", nil, "", imgTestAuth{})
	if meta.Code != http.StatusOK {
		t.Fatalf("读取 meta 失败: status=%d body=%s", meta.Code, meta.Body.String())
	}
	metaBody := imgTestJSON(t, meta)
	imageMeta, _ := metaBody["image"].(map[string]any)
	if width, _ := imageMeta["width"].(float64); int(width) != 20 {
		t.Fatalf("meta.width = %v，期望画布宽 20", imageMeta["width"])
	}
	if height, _ := imageMeta["height"].(float64); int(height) != 16 {
		t.Fatalf("meta.height = %v，期望画布高 16", imageMeta["height"])
	}

	// 缩略图取首帧，画布的大小与偏移不变；第二帧不在缩略图里。
	thumb := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=thumb", nil, "", imgTestAuth{})
	if thumb.Code != http.StatusOK {
		t.Fatalf("读取缩略图失败: status=%d body=%s", thumb.Code, thumb.Body.String())
	}
	decoded, _, err := image.Decode(bytes.NewReader(thumb.Body.Bytes()))
	if err != nil {
		t.Fatalf("缩略图无法解码: %v", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 20 || bounds.Dy() != 16 {
		t.Fatalf("缩略图尺寸 = %dx%d，期望画布 20x16", bounds.Dx(), bounds.Dy())
	}
	transparent := color.NRGBA{}
	imgTestExpectPixel(t, decoded, 0, 0, transparent)
	imgTestExpectPixel(t, decoded, 7, 3, transparent)
	imgTestExpectPixel(t, decoded, 8, 4, color.NRGBA{255, 0, 0, 128})
	imgTestExpectPixel(t, decoded, 15, 11, color.NRGBA{255, 0, 0, 128})
	imgTestExpectPixel(t, decoded, 16, 12, transparent)
	imgTestExpectPixel(t, decoded, 19, 15, transparent)
}

func TestImagesAnimatedWebPLossyAlphaFrame(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "animated-alpha@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	// 首帧是 8x8 有损 VP8 + ALPH（左半不透明红、右半透明），放在画布 (2,2)；
	// ANIM 背景是不透明绿，但首帧画布必须从透明开始，不填背景色。
	lossy, err := base64.StdEncoding.DecodeString(imgTestLossyAlphaWebPBase64)
	if err != nil {
		t.Fatalf("解码内嵌有损样本失败: %v", err)
	}
	lossyChunks := imgTestWebPChunks(t, lossy)
	firstFrame := imgTestWebPAnmf(2, 2, 8, 8, 0,
		imgTestWebPChunk("ALPH", lossyChunks["ALPH"]),
		imgTestWebPChunk("VP8 ", lossyChunks["VP8 "]),
	)
	animated := imgTestWebPFile(
		imgTestWebPAnimatedHeader(12, 10, 0x12, 0xFF00FF00, 0),
		imgTestWebPChunk("ANMF", firstFrame),
		imgTestWebPChunk("ANMF", imgTestWebPAnmf(0, 0, 8, 8, 0,
			imgTestWebPFrameChunks(t, imgTestSolidImage(image.Rect(0, 0, 8, 8), color.NRGBA{0, 0, 255, 255}))...)),
	)

	recorder, body := env.imgTestUploadBytes(t, auth, "animated-alpha.webp", animated, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imageID := imgTestMustImageID(t, body)

	thumb := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=thumb", nil, "", imgTestAuth{})
	if thumb.Code != http.StatusOK {
		t.Fatalf("读取缩略图失败: status=%d body=%s", thumb.Code, thumb.Body.String())
	}
	decoded, _, err := image.Decode(bytes.NewReader(thumb.Body.Bytes()))
	if err != nil {
		t.Fatalf("缩略图无法解码: %v", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 12 || bounds.Dy() != 10 {
		t.Fatalf("缩略图尺寸 = %dx%d，期望画布 12x10", bounds.Dx(), bounds.Dy())
	}
	transparent := color.NRGBA{}
	imgTestExpectPixel(t, decoded, 0, 0, transparent)
	imgTestExpectPixel(t, decoded, 1, 1, transparent)
	imgTestExpectPixel(t, decoded, 11, 9, transparent)
	// ALPH 的 0/255 必须精确保留；有损 VP8 只对 RGB 做容差比较。
	imgTestExpectPixelNear(t, decoded, 2, 2, color.NRGBA{255, 0, 0, 255}, 30)
	imgTestExpectPixelNear(t, decoded, 5, 5, color.NRGBA{255, 0, 0, 255}, 40)
	imgTestExpectPixel(t, decoded, 6, 5, transparent)
	imgTestExpectPixel(t, decoded, 9, 9, transparent)

	// 有损帧也只保存原图字节。
	original := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=original", nil, "", imgTestAuth{})
	if original.Code != http.StatusOK || !bytes.Equal(original.Body.Bytes(), animated) {
		t.Fatalf("有损动画原图字节未保留: status=%d len=%d", original.Code, original.Body.Len())
	}
}

func TestImagesAnimatedWebPLossyRawAlphaFrame(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "animated-raw-alpha@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	lossy, err := base64.StdEncoding.DecodeString(imgTestLossyAlphaWebPBase64)
	if err != nil {
		t.Fatalf("解码内嵌有损样本失败: %v", err)
	}
	lossyChunks := imgTestWebPChunks(t, lossy)
	// 压缩方式 0 的 ALPH：1 字节头 + 每像素 1 字节。左半不透明、右半透明。
	rawAlpha := make([]byte, 1+8*8)
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if x < 4 {
				rawAlpha[1+y*8+x] = 255
			}
		}
	}
	firstFrame := imgTestWebPAnmf(2, 2, 8, 8, 0,
		imgTestWebPChunk("ALPH", rawAlpha),
		imgTestWebPChunk("VP8 ", lossyChunks["VP8 "]),
	)
	animated := imgTestWebPFile(
		imgTestWebPAnimatedHeader(12, 10, 0x12, 0xFF00FF00, 0),
		imgTestWebPChunk("ANMF", firstFrame),
	)

	recorder, body := env.imgTestUploadBytes(t, auth, "animated-raw-alpha.webp", animated, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imageID := imgTestMustImageID(t, body)
	thumb := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=thumb", nil, "", imgTestAuth{})
	if thumb.Code != http.StatusOK {
		t.Fatalf("读取缩略图失败: status=%d body=%s", thumb.Code, thumb.Body.String())
	}
	decoded, _, err := image.Decode(bytes.NewReader(thumb.Body.Bytes()))
	if err != nil {
		t.Fatalf("缩略图无法解码: %v", err)
	}
	imgTestExpectPixel(t, decoded, 0, 0, color.NRGBA{})
	imgTestExpectPixelNear(t, decoded, 2, 2, color.NRGBA{255, 0, 0, 255}, 40)
	imgTestExpectPixelNear(t, decoded, 5, 5, color.NRGBA{255, 0, 0, 255}, 40)
	imgTestExpectPixel(t, decoded, 6, 5, color.NRGBA{})
}

func TestImagesAnimatedWebPThumbnailScalesLargeCanvas(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "animated-large@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	animated := imgTestAnimatedWebPWithCanvas(t, image.Rect(0, 0, 500, 250), 2)
	recorder, body := env.imgTestUploadBytes(t, auth, "animated-large.webp", animated, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imageID := imgTestMustImageID(t, body)

	thumb := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=thumb", nil, "", imgTestAuth{})
	if thumb.Code != http.StatusOK {
		t.Fatalf("读取缩略图失败: status=%d body=%s", thumb.Code, thumb.Body.String())
	}
	decoded, _, err := image.Decode(bytes.NewReader(thumb.Body.Bytes()))
	if err != nil {
		t.Fatalf("缩略图无法解码: %v", err)
	}
	if bounds := decoded.Bounds(); bounds.Dx() != 400 || bounds.Dy() != 200 {
		t.Fatalf("缩略图尺寸 = %dx%d，期望 400x200", bounds.Dx(), bounds.Dy())
	}
}

func TestImagesAnimatedWebPLaterFrameValidation(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "animated-later@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	first := imgTestWebPFrameChunks(t, imgTestSolidImage(image.Rect(0, 0, 8, 8), color.NRGBA{10, 200, 10, 255}))
	secondImage := imgTestSolidImage(image.Rect(8, 4, 16, 12), color.NRGBA{200, 10, 10, 255})
	var encoded bytes.Buffer
	if err := nativewebp.Encode(&encoded, secondImage, nil); err != nil {
		t.Fatalf("生成第二帧失败: %v", err)
	}
	secondChunks := imgTestWebPChunks(t, encoded.Bytes())
	secondBits := bytes.Clone(secondChunks["VP8L"])

	deepCorrupt := bytes.Clone(secondBits)
	deepCorrupt[len(deepCorrupt)-1] ^= 0xff
	headerCorrupt := bytes.Clone(secondBits)
	headerCorrupt[0] = 0xff // VP8L 签名应为 0x2f

	canvas := [][]byte{
		imgTestWebPAnimatedHeader(20, 16, 0x12, 0, 0),
		imgTestWebPChunk("ANMF", imgTestWebPAnmf(0, 0, 8, 8, 0, first...)),
	}

	t.Run("accepts-deep-bitstream-corruption", func(t *testing.T) {
		// 后续帧的位流头合法、深处损坏：只做轻量头校验时允许保存；
		// 首帧仍然只解码一次，并与第二帧无关。
		animated := imgTestWebPFile(
			canvas[0],
			canvas[1],
			imgTestWebPChunk("ANMF", imgTestWebPAnmf(8, 4, 8, 8, 0, imgTestWebPChunk("VP8L", deepCorrupt))),
		)
		recorder, body := env.imgTestUploadBytes(t, auth, "animated-later.webp", animated, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("后续帧不应被完整解码: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		imageID := imgTestMustImageID(t, body)
		thumb := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=thumb", nil, "", imgTestAuth{})
		decoded, _, err := image.Decode(bytes.NewReader(thumb.Body.Bytes()))
		if err != nil {
			t.Fatalf("缩略图无法解码: %v", err)
		}
		imgTestExpectPixel(t, decoded, 0, 0, color.NRGBA{10, 200, 10, 255})
	})

	t.Run("rejects-corrupt-frame-header", func(t *testing.T) {
		// 位流签名损坏：sharp/libwebp 也会在帧头处失败，这里必须在保存前拒绝。
		animated := imgTestWebPFile(
			canvas[0],
			canvas[1],
			imgTestWebPChunk("ANMF", imgTestWebPAnmf(8, 4, 8, 8, 0, imgTestWebPChunk("VP8L", headerCorrupt))),
		)
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "animated-later.webp", Data: animated})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("损坏的后续帧头 status=%d，期望 400 body=%s", recorder.Code, recorder.Body.String())
		}
		if code := imgTestErrorCode(t, recorder); code != "INVALID_IMAGE" {
			t.Fatalf("error.code = %q，期望 INVALID_IMAGE", code)
		}
	})
}

func TestImagesAnimatedWebPRejectsInvalidContainers(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "animated-reject@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	lossy, err := base64.StdEncoding.DecodeString(imgTestLossyAlphaWebPBase64)
	if err != nil {
		t.Fatalf("解码内嵌有损样本失败: %v", err)
	}
	lossyChunks := imgTestWebPChunks(t, lossy)
	solid := imgTestWebPFrameChunks(t, imgTestSolidImage(image.Rect(0, 0, 8, 8), color.NRGBA{200, 30, 30, 255}))
	tiny := imgTestWebPFrameChunks(t, imgTestSolidImage(image.Rect(0, 0, 4, 4), color.NRGBA{30, 30, 200, 255}))

	header := imgTestWebPAnimatedHeader(20, 16, 0x12, 0xFF00FF00, 0)
	valid := imgTestWebPFile(header, imgTestWebPChunk("ANMF", imgTestWebPAnmf(8, 4, 8, 8, 0, solid...)))
	// 'ANIM' 长度为 5 字节（非法）。
	badAnim := bytes.Join([][]byte{
		imgTestWebPChunk("VP8X", make([]byte, 10)),
		imgTestWebPChunk("ANIM", make([]byte, 5)),
	}, nil)
	badAnim[12+8] = 0x12 // 画布 1x1，仅用于走到 ANIM 校验

	withoutPadding := imgTestWebPAnmfHeader(2, 2, 8, 8, 0)
	withoutPadding = append(withoutPadding, []byte("ALPH")...)
	withoutPadding = append(withoutPadding, byte(len(lossyChunks["ALPH"])), 0, 0, 0)
	withoutPadding = append(withoutPadding, lossyChunks["ALPH"]...)
	withoutPadding = append(withoutPadding, imgTestWebPChunk("VP8 ", lossyChunks["VP8 "])...)

	subchunkOverrun := imgTestWebPAnmfHeader(0, 0, 8, 8, 0)
	subchunkOverrun = append(subchunkOverrun, []byte{'V', 'P', '8', 'L', 0xff, 0xff, 0x00, 0x00, 1, 2, 3}...)

	duplicateBits := append(append([][]byte{}, solid...), solid...)
	// ALPH 子块后接无损位流：两者不能同时出现。
	alphWithLossless := append([][]byte{imgTestWebPChunk("ALPH", lossyChunks["ALPH"])}, solid...)

	riffTooLong := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(riffTooLong[4:8], 0x7fffffff)
	anmfTooLong := bytes.Clone(valid)
	binary.LittleEndian.PutUint32(anmfTooLong[12+len(header)+4:12+len(header)+8], 0xffffff00)

	cases := []struct {
		name   string
		data   []byte
		code   string
		status int
	}{
		{
			name: "huge-canvas",
			data: imgTestWebPFile(
				imgTestWebPAnimatedHeader(100000, 1000, 0x12, 0, 0),
				imgTestWebPChunk("ANMF", imgTestWebPAnmf(0, 0, 8, 8, 0, solid...)),
			),
			code: "IMAGE_TOO_LARGE", status: http.StatusBadRequest,
		},
		{
			name: "frame-outside-canvas",
			data: imgTestWebPFile(header, imgTestWebPChunk("ANMF", imgTestWebPAnmf(16, 8, 8, 8, 0, solid...))),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "frame-size-mismatch",
			data: imgTestWebPFile(header, imgTestWebPChunk("ANMF", imgTestWebPAnmf(0, 0, 8, 8, 0, tiny...))),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "later-frame-size-mismatch",
			data: imgTestWebPFile(
				header,
				imgTestWebPChunk("ANMF", imgTestWebPAnmf(0, 0, 8, 8, 0, solid...)),
				imgTestWebPChunk("ANMF", imgTestWebPAnmf(8, 4, 8, 8, 0, tiny...)),
			),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "anmf-too-short",
			data: imgTestWebPFile(header, imgTestWebPChunk("ANMF", make([]byte, 10))),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "missing-anim",
			data: imgTestWebPFile(
				imgTestWebPChunk("VP8X", []byte{0x12, 0, 0, 0, 19, 0, 0, 15, 0, 0}),
				imgTestWebPChunk("ANMF", imgTestWebPAnmf(8, 4, 8, 8, 0, solid...)),
			),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "bad-anim-length",
			data: imgTestWebPFile(badAnim, imgTestWebPChunk("ANMF", imgTestWebPAnmf(0, 0, 1, 1, 0, solid...))),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "raw-alpha-too-short",
			data: imgTestWebPFile(header, imgTestWebPChunk("ANMF", imgTestWebPAnmf(2, 2, 8, 8, 0,
				imgTestWebPChunk("ALPH", append([]byte{0}, bytes.Repeat([]byte{0xff}, 10)...)),
				imgTestWebPChunk("VP8 ", lossyChunks["VP8 "]),
			))),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "missing-bitstream",
			data: imgTestWebPFile(header, imgTestWebPChunk("ANMF", imgTestWebPAnmf(2, 2, 8, 8, 0, imgTestWebPChunk("ALPH", lossyChunks["ALPH"])))),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "duplicate-bitstream",
			data: imgTestWebPFile(header, imgTestWebPChunk("ANMF", imgTestWebPAnmf(0, 0, 8, 8, 0, duplicateBits...))),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "alph-with-lossless",
			data: imgTestWebPFile(header, imgTestWebPChunk("ANMF", imgTestWebPAnmf(2, 2, 8, 8, 0, alphWithLossless...))),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "subchunk-overrun",
			data: imgTestWebPFile(header, imgTestWebPChunk("ANMF", subchunkOverrun)),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "missing-subchunk-padding",
			data: imgTestWebPFile(header, imgTestWebPChunk("ANMF", withoutPadding)),
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "truncated-tail",
			data: valid[:len(valid)-5],
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "riff-length-exceeds-file",
			data: riffTooLong,
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
		{
			name: "anmf-length-exceeds-file",
			data: anmfTooLong,
			code: "INVALID_IMAGE", status: http.StatusBadRequest,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "animated.webp", Data: testCase.data})
			if recorder.Code != testCase.status {
				t.Fatalf("status = %d，期望 %d，body=%s", recorder.Code, testCase.status, recorder.Body.String())
			}
			if code := imgTestErrorCode(t, recorder); code != testCase.code {
				t.Fatalf("error.code = %q，期望 %q，body=%s", code, testCase.code, recorder.Body.String())
			}
			if entries := imgTestImagesDirEntries(t, env.app.cfg.DataDir); len(entries) != 0 {
				t.Fatalf("拒绝后残留文件: %v", entries)
			}
		})
	}
}

func TestImagesUploadRejectsInvalidInputs(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "reject@example.com", "password123")
	auth := imgTestCookieAuth(cookie)
	validPNG := imgTestPNG(t, 40, 30)

	expect := func(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		if recorder.Code != status {
			t.Fatalf("status = %d，期望 %d，body=%s", recorder.Code, status, recorder.Body.String())
		}
		if got := imgTestErrorCode(t, recorder); got != code {
			t.Fatalf("error.code = %q，期望 %q，body=%s", got, code, recorder.Body.String())
		}
	}

	t.Run("svg-rejected", func(t *testing.T) {
		svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"><rect width="10" height="10"/></svg>`)
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "vector.svg", Data: svg})
		expect(t, recorder, http.StatusBadRequest, "INVALID_FILE_TYPE")
	})

	t.Run("unknown-bytes-rejected", func(t *testing.T) {
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "note.txt", Data: []byte("hello world")})
		expect(t, recorder, http.StatusBadRequest, "INVALID_FILE_TYPE")
	})

	t.Run("corrupt-png-rejected", func(t *testing.T) {
		corrupt := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0x42}, 64)...)
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "broken.png", Data: corrupt})
		expect(t, recorder, http.StatusBadRequest, "INVALID_IMAGE")
	})

	t.Run("truncated-jpeg-rejected", func(t *testing.T) {
		truncated := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "broken.jpg", Data: truncated})
		expect(t, recorder, http.StatusBadRequest, "INVALID_IMAGE")
	})

	t.Run("animated-webp-corrupt-frame", func(t *testing.T) {
		// 动画位存在但首帧位流损坏：必须走 INVALID_IMAGE，且不留下文件。
		broken := imgTestWebPFile(
			imgTestWebPAnimatedHeader(64, 48, 0x12, 0, 0),
			imgTestWebPChunk("ANMF", imgTestWebPAnmf(0, 0, 64, 48, 0, imgTestWebPChunk("VP8L", bytes.Repeat([]byte{0x7f}, 40)))),
		)
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "animated.webp", Data: broken})
		expect(t, recorder, http.StatusBadRequest, "INVALID_IMAGE")
		if entries := imgTestImagesDirEntries(t, env.app.cfg.DataDir); len(entries) != 0 {
			t.Fatalf("损坏动画残留文件: %v", entries)
		}
	})

	t.Run("too-many-pixels", func(t *testing.T) {
		small := imgTestNewApp(t, Config{MaxImagePixels: 100})
		smallCookie := small.registerAdmin(t, "pixels@example.com", "password123")
		recorder := small.upload(t, imgTestCookieAuth(smallCookie), imgTestUploadOptions{FileName: "big.png", Data: imgTestPNG(t, 20, 20)})
		expect(t, recorder, http.StatusBadRequest, "IMAGE_TOO_LARGE")
		if entries := imgTestImagesDirEntries(t, small.app.cfg.DataDir); len(entries) != 0 {
			t.Fatalf("像素超限后残留文件: %v", entries)
		}
	})

	t.Run("huge-dimensions", func(t *testing.T) {
		// 100000x100000 pixels is accepted by the PNG header decoder but exceeds the
		// default 16MP cap; the guard must reject it before Decode.
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "huge.png", Data: imgTestPNGHeaderOnly(100000, 100000)})
		expect(t, recorder, http.StatusBadRequest, "IMAGE_TOO_LARGE")
	})

	t.Run("dimension-guard-does-not-overflow", func(t *testing.T) {
		// int32 extremes multiplied naively would overflow int64; the guard divides
		// instead, so this must report an error rather than panic or pass.
		if err := imagesCheckDimensions(1<<31-1, 1<<31-1, 100); err == nil {
			t.Fatalf("极端尺寸未被拒绝")
		}
	})

	t.Run("file-too-large", func(t *testing.T) {
		small := imgTestNewApp(t, Config{UploadMaxBytes: 1024})
		smallCookie := small.registerAdmin(t, "toobig@example.com", "password123")
		recorder := small.upload(t, imgTestCookieAuth(smallCookie), imgTestUploadOptions{FileName: "big.png", Data: imgTestPNG(t, 300, 300)})
		expect(t, recorder, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE")
		if entries := imgTestImagesDirEntries(t, small.app.cfg.DataDir); len(entries) != 0 {
			t.Fatalf("超限上传残留文件: %v", entries)
		}
	})

	t.Run("too-many-fields", func(t *testing.T) {
		fields := make([]imgTestFormField, 0, imagesMaxFields+1)
		for index := 0; index < imagesMaxFields+1; index++ {
			fields = append(fields, imgTestFormField{Name: fmt.Sprintf("field%d", index), Value: "x"})
		}
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "fields.png", Data: validPNG, Fields: fields})
		expect(t, recorder, http.StatusBadRequest, "INVALID_REQUEST")
	})

	t.Run("oversized-field", func(t *testing.T) {
		fields := []imgTestFormField{{Name: "tags", Value: strings.Repeat("a", imagesMaxFieldBytes+10)}}
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "field.png", Data: validPNG, Fields: fields})
		expect(t, recorder, http.StatusBadRequest, "INVALID_REQUEST")
	})

	t.Run("two-files", func(t *testing.T) {
		recorder := env.upload(t, auth, imgTestUploadOptions{
			FileName:   "first.png",
			Data:       validPNG,
			ExtraFiles: []imgTestFormField{{Name: "file", Value: "second.png"}},
		})
		expect(t, recorder, http.StatusBadRequest, "INVALID_REQUEST")
	})

	t.Run("missing-file", func(t *testing.T) {
		recorder := env.upload(t, auth, imgTestUploadOptions{Fields: []imgTestFormField{{Name: "tags", Value: `[]`}}})
		expect(t, recorder, http.StatusBadRequest, "INVALID_FILE")
	})

	t.Run("tag-and-tags-mixed", func(t *testing.T) {
		fields := []imgTestFormField{
			{Name: "tag", Value: "legacy"},
			{Name: "tags", Value: `["one"]`},
		}
		recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "mixed.png", Data: validPNG, Fields: fields})
		expect(t, recorder, http.StatusBadRequest, "INVALID_TAG")
	})

	t.Run("too-many-tags", func(t *testing.T) {
		tags := make([]string, 0, imagesMaxTags+1)
		for index := 0; index < imagesMaxTags+1; index++ {
			tags = append(tags, fmt.Sprintf("tag-%02d", index))
		}
		recorder := env.upload(t, auth, imgTestUploadOptions{
			FileName: "tags.png", Data: validPNG,
			Fields: []imgTestFormField{{Name: "tags", Value: imgTestTagsJSON(t, tags)}},
		})
		expect(t, recorder, http.StatusBadRequest, "INVALID_TAG")
	})

	t.Run("tag-too-long", func(t *testing.T) {
		recorder := env.upload(t, auth, imgTestUploadOptions{
			FileName: "longtag.png", Data: validPNG,
			Fields: []imgTestFormField{{Name: "tag", Value: strings.Repeat("宽", imagesMaxTagLength+1)}},
		})
		expect(t, recorder, http.StatusBadRequest, "INVALID_TAG")
	})

	t.Run("tag-control-character", func(t *testing.T) {
		recorder := env.upload(t, auth, imgTestUploadOptions{
			FileName: "control.png", Data: validPNG,
			Fields: []imgTestFormField{{Name: "tag", Value: "bad\u0001tag"}},
		})
		expect(t, recorder, http.StatusBadRequest, "INVALID_TAG")
	})

	t.Run("tags-not-json-array", func(t *testing.T) {
		recorder := env.upload(t, auth, imgTestUploadOptions{
			FileName: "json.png", Data: validPNG,
			Fields: []imgTestFormField{{Name: "tags", Value: `{"a":1}`}},
		})
		expect(t, recorder, http.StatusBadRequest, "INVALID_TAG")
	})

	t.Run("unauthenticated", func(t *testing.T) {
		recorder := env.upload(t, imgTestAuth{}, imgTestUploadOptions{FileName: "anon.png", Data: validPNG})
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d，期望 401", recorder.Code)
		}
	})

	t.Run("wrong-content-type", func(t *testing.T) {
		recorder := env.do(t, http.MethodPost, "/api/images", strings.NewReader(`{}`), "application/json", auth)
		expect(t, recorder, http.StatusBadRequest, "INVALID_REQUEST")
	})

	t.Run("original-name-is-sanitized", func(t *testing.T) {
		recorder, body := env.imgTestUploadBytes(t, auth, "../../evil.png", validPNG, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		imageID := imgTestMustImageID(t, body)
		var originalName string
		if err := env.app.db.QueryRow(`SELECT originalName FROM "Image" WHERE id = ?`, imageID).Scan(&originalName); err != nil {
			t.Fatalf("读取 originalName 失败: %v", err)
		}
		if originalName != "evil.png" {
			t.Fatalf("originalName = %q，期望 evil.png", originalName)
		}
	})
}

func TestImagesUploadBusySlotReturns429WithoutQueueing(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "busy@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	// Occupy the only processing slot, then expect an immediate 429.
	env.app.uploadSlots <- struct{}{}
	recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "busy.png", Data: imgTestPNG(t, 40, 30)})
	<-env.app.uploadSlots
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("满载上传 status = %d，期望 429，body=%s", recorder.Code, recorder.Body.String())
	}
	if got := imgTestErrorCode(t, recorder); got != "UPLOAD_BUSY" {
		t.Fatalf("error.code = %q，期望 UPLOAD_BUSY", got)
	}

	// The slot is available again, so a retry succeeds.
	recorder, body := env.imgTestUploadBytes(t, auth, "retry.png", imgTestPNG(t, 40, 30), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("重试上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imgTestMustImageID(t, body)
}

func TestImagesQuotaCountsTrashedImages(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "quota@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	fixture := imgTestPNG(t, 60, 40)
	quota := int64(len(fixture)) + 8
	env.setQuota(t, 1, quota)

	first, firstBody := env.imgTestUploadBytes(t, auth, "first.png", fixture, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("首次上传失败: status=%d body=%s", first.Code, first.Body.String())
	}
	firstID := imgTestMustImageID(t, firstBody)

	// Trash still occupies quota: deleting must not free the bytes.
	imgTestTrash(t, env, auth, firstID)

	second := env.upload(t, auth, imgTestUploadOptions{FileName: "second.png", Data: fixture})
	if second.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超配额上传 status = %d，期望 413，body=%s", second.Code, second.Body.String())
	}
	if got := imgTestErrorCode(t, second); got != "USER_QUOTA_EXCEEDED" {
		t.Fatalf("error.code = %q，期望 USER_QUOTA_EXCEEDED", got)
	}
	if count := imgTestImageCount(t, env); count != 1 {
		t.Fatalf("图片行数 = %d，期望 1", count)
	}
	if entries := imgTestImagesDirEntries(t, env.app.cfg.DataDir); len(entries) != 2 {
		t.Fatalf("超配额上传残留文件: %v", entries)
	}

	// Unlimited quota accepts the same upload.
	env.setQuota(t, 1, 0)
	third := env.upload(t, auth, imgTestUploadOptions{FileName: "third.png", Data: fixture})
	if third.Code != http.StatusOK {
		t.Fatalf("解除配额后上传失败: status=%d body=%s", third.Code, third.Body.String())
	}
}

func TestImagesFailedInsertCleansUpFiles(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "rollback@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	if _, err := env.app.db.Exec(`CREATE TRIGGER img_test_fail_insert BEFORE INSERT ON "Image"
		BEGIN SELECT RAISE(ABORT, 'forced insert failure'); END`); err != nil {
		t.Fatalf("创建失败触发器失败: %v", err)
	}

	recorder := env.upload(t, auth, imgTestUploadOptions{FileName: "rollback.png", Data: imgTestPNG(t, 50, 40)})
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("事务失败时 status = %d，期望 500，body=%s", recorder.Code, recorder.Body.String())
	}
	if count := imgTestImageCount(t, env); count != 0 {
		t.Fatalf("事务失败后仍写入 %d 行", count)
	}
	if entries := imgTestImagesDirEntries(t, env.app.cfg.DataDir); len(entries) != 0 {
		t.Fatalf("事务失败后残留文件: %v", entries)
	}
	entries, err := os.ReadDir(filepath.Join(env.app.cfg.DataDir, imagesTempDirName))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("读取暂存目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("事务失败后暂存目录残留: %v", entries)
	}
}

func TestImagesListPaginationTagsAndSort(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "list@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	type fixture struct {
		id   string
		name string
	}
	fixtures := make([]fixture, 0, 3)
	uploads := []struct {
		name string
		data []byte
		tags []string
	}{
		{name: "alpha.png", data: imgTestPNG(t, 90, 60), tags: []string{"red", "blue"}},
		{name: "beta.png", data: imgTestPNG(t, 30, 20), tags: []string{"red"}},
		{name: "gamma.png", data: imgTestPNG(t, 60, 45), tags: []string{"green"}},
	}
	for _, upload := range uploads {
		recorder, body := env.imgTestUploadBytes(t, auth, upload.name, upload.data, upload.tags)
		if recorder.Code != http.StatusOK {
			t.Fatalf("上传 %s 失败: status=%d body=%s", upload.name, recorder.Code, recorder.Body.String())
		}
		// Distinct createdAt so the date cursor has stable ordering.
		time.Sleep(2 * time.Millisecond)
		fixtures = append(fixtures, fixture{id: imgTestMustImageID(t, body), name: upload.name})
	}
	alpha, beta, gamma := fixtures[0].id, fixtures[1].id, fixtures[2].id

	t.Run("pagination-is-stable", func(t *testing.T) {
		first := env.do(t, http.MethodGet, "/api/images?mine=true&limit=2", nil, "", auth)
		if first.Code != http.StatusOK {
			t.Fatalf("列表失败: status=%d body=%s", first.Code, first.Body.String())
		}
		body := imgTestJSON(t, first)
		if total, _ := body["total"].(float64); total != 3 {
			t.Fatalf("total = %v，期望 3", body["total"])
		}
		ids := imgTestListIDs(t, first)
		if len(ids) != 2 {
			t.Fatalf("第一页条数 = %d，期望 2", len(ids))
		}
		if ids[0] != gamma || ids[1] != beta {
			t.Fatalf("newest 顺序错误: %v", ids)
		}
		cursor, _ := body["nextCursor"].(string)
		if cursor == "" {
			t.Fatalf("第一页应返回 nextCursor: %s", first.Body.String())
		}
		second := env.do(t, http.MethodGet, "/api/images?mine=true&limit=2&cursor="+cursor, nil, "", auth)
		secondIDs := imgTestListIDs(t, second)
		if len(secondIDs) != 1 || secondIDs[0] != alpha {
			t.Fatalf("第二页错误: %v", secondIDs)
		}
		if nextCursor, _ := imgTestJSON(t, second)["nextCursor"]; nextCursor != nil {
			t.Fatalf("最后一页 nextCursor 应为 null，实际 %v", nextCursor)
		}
	})

	t.Run("legacy-date-cursor", func(t *testing.T) {
		// The previous implementation emitted a bare ISO timestamp; it must still page.
		first := env.do(t, http.MethodGet, "/api/images?mine=true&limit=2", nil, "", auth)
		body := imgTestJSON(t, first)
		rawItems, _ := body["items"].([]any)
		item, _ := rawItems[1].(map[string]any)
		legacyCursor, _ := item["createdAt"].(string)
		if legacyCursor == "" {
			t.Fatalf("条目缺少 createdAt: %v", item)
		}
		second := env.do(t, http.MethodGet, "/api/images?mine=true&limit=2&cursor="+legacyCursor, nil, "", auth)
		if ids := imgTestListIDs(t, second); len(ids) != 1 || ids[0] != alpha {
			t.Fatalf("旧格式游标分页错误: %v", ids)
		}
	})

	t.Run("garbage-cursor-starts-from-first-page", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?mine=true&limit=2&cursor=not-a-date", nil, "", auth)
		if ids := imgTestListIDs(t, recorder); len(ids) != 2 || ids[0] != gamma {
			t.Fatalf("非法游标应回落到第一页: %v", ids)
		}
	})

	t.Run("limit-is-clamped", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?mine=true&limit=500", nil, "", auth)
		if recorder.Code != http.StatusOK {
			t.Fatalf("列表失败: status=%d", recorder.Code)
		}
		if ids := imgTestListIDs(t, recorder); len(ids) != 3 {
			t.Fatalf("条数 = %d，期望 3", len(ids))
		}
		// invalid limit falls back to the default instead of failing
		recorder = env.do(t, http.MethodGet, "/api/images?mine=true&limit=abc", nil, "", auth)
		if recorder.Code != http.StatusOK {
			t.Fatalf("非法 limit status = %d，期望 200", recorder.Code)
		}
	})

	t.Run("sort-name", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?mine=true&sort=name", nil, "", auth)
		ids := imgTestListIDs(t, recorder)
		if len(ids) != 3 || ids[0] != alpha || ids[1] != beta || ids[2] != gamma {
			t.Fatalf("name 排序错误: %v", ids)
		}
		// Offset cursor works for name/size sorts.
		cursor, _ := imgTestJSON(t, recorder)["nextCursor"].(string)
		if cursor != "" {
			t.Fatalf("未达到 limit 时不应返回 nextCursor: %s", cursor)
		}
		first := env.do(t, http.MethodGet, "/api/images?mine=true&sort=name&limit=2", nil, "", auth)
		body := imgTestJSON(t, first)
		cursor, _ = body["nextCursor"].(string)
		if cursor != "o:2" {
			t.Fatalf("offset 游标 = %q，期望 o:2", cursor)
		}
		next := env.do(t, http.MethodGet, "/api/images?mine=true&sort=name&limit=2&cursor="+cursor, nil, "", auth)
		if ids := imgTestListIDs(t, next); len(ids) != 1 || ids[0] != gamma {
			t.Fatalf("offset 分页错误: %v", ids)
		}
	})

	t.Run("sort-size", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?mine=true&sort=size", nil, "", auth)
		ids := imgTestListIDs(t, recorder)
		if len(ids) != 3 {
			t.Fatalf("条数 = %d，期望 3", len(ids))
		}
		// alpha (90x60) is the largest fixture.
		if ids[0] != alpha {
			t.Fatalf("size 排序错误: %v", ids)
		}
	})

	t.Run("invalid-sort", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?mine=true&sort=bogus", nil, "", auth)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400", recorder.Code)
		}
		if got := imgTestErrorCode(t, recorder); got != "INVALID_SORT" {
			t.Fatalf("code = %q", got)
		}
	})

	t.Run("tag-filter-any-and-all", func(t *testing.T) {
		anyRecorder := env.do(t, http.MethodGet, "/api/images?mine=true&tag=red", nil, "", auth)
		if ids := imgTestListIDs(t, anyRecorder); len(ids) != 2 {
			t.Fatalf("tag=red 条数 = %d，期望 2", len(ids))
		}
		allRecorder := env.do(t, http.MethodGet, "/api/images?mine=true&tag=red&tag=blue&tagMatch=all", nil, "", auth)
		if ids := imgTestListIDs(t, allRecorder); len(ids) != 1 || ids[0] != alpha {
			t.Fatalf("tagMatch=all 结果错误: %v", ids)
		}
		anyTwo := env.do(t, http.MethodGet, "/api/images?mine=true&tag=blue&tag=green", nil, "", auth)
		if ids := imgTestListIDs(t, anyTwo); len(ids) != 2 {
			t.Fatalf("多标签 any 条数 = %d，期望 2", len(ids))
		}
	})

	t.Run("tag-filter-requires-mine", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?tag=red", nil, "", imgTestAuth{})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400，body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("invalid-tag-match", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?mine=true&tagMatch=bogus", nil, "", auth)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400", recorder.Code)
		}
	})

	t.Run("query-filter", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?mine=true&query=bet", nil, "", auth)
		if ids := imgTestListIDs(t, recorder); len(ids) != 1 || ids[0] != beta {
			t.Fatalf("query 结果错误: %v", ids)
		}
	})

	t.Run("visibility-filter", func(t *testing.T) {
		imgTestSetHidden(t, env, auth, beta, false)
		hidden := env.do(t, http.MethodGet, "/api/images?mine=true&visibility=hidden", nil, "", auth)
		if ids := imgTestListIDs(t, hidden); len(ids) != 1 || ids[0] != beta {
			t.Fatalf("visibility=hidden 结果错误: %v", ids)
		}
		visible := env.do(t, http.MethodGet, "/api/images?mine=true&visibility=visible", nil, "", auth)
		if len(imgTestListIDs(t, visible)) != 2 {
			t.Fatalf("visibility=visible 结果错误: %s", visible.Body.String())
		}
		all := env.do(t, http.MethodGet, "/api/images?mine=true", nil, "", auth)
		if len(imgTestListIDs(t, all)) != 3 {
			t.Fatalf("个人列表应包含隐藏图片: %s", all.Body.String())
		}
		// Public list never includes the hidden image.
		public := env.do(t, http.MethodGet, "/api/images", nil, "", imgTestAuth{})
		if ids := imgTestListIDs(t, public); len(ids) != 2 {
			t.Fatalf("公开列表条数 = %d，期望 2", len(ids))
		}
		publicHidden := env.do(t, http.MethodGet, "/api/images?visibility=hidden", nil, "", imgTestAuth{})
		if ids := imgTestListIDs(t, publicHidden); len(ids) != 0 {
			t.Fatalf("公开列表 visibility=hidden 应为空: %v", ids)
		}
		if !strings.Contains(publicHidden.Body.String(), `"items":[]`) {
			t.Fatalf("空列表必须是 [] 而不是 null: %s", publicHidden.Body.String())
		}
	})

	t.Run("trash-list", func(t *testing.T) {
		imgTestTrash(t, env, auth, gamma)
		trashed := env.do(t, http.MethodGet, "/api/images?mine=true&deleted=true", nil, "", auth)
		if ids := imgTestListIDs(t, trashed); len(ids) != 1 || ids[0] != gamma {
			t.Fatalf("回收站结果错误: %v", ids)
		}
		body := imgTestJSON(t, trashed)
		rawItems, _ := body["items"].([]any)
		item, _ := rawItems[0].(map[string]any)
		if item["deletedAt"] == nil {
			t.Fatalf("回收站条目缺少 deletedAt: %v", item)
		}
		active := env.do(t, http.MethodGet, "/api/images?mine=true", nil, "", auth)
		if ids := imgTestListIDs(t, active); len(ids) != 2 {
			t.Fatalf("个人列表应排除回收站: %v", ids)
		}
	})

	t.Run("trash-requires-mine", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?deleted=true", nil, "", imgTestAuth{})
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，期望 400", recorder.Code)
		}
	})

	t.Run("mine-requires-cookie", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?mine=true", nil, "", imgTestAuth{})
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d，期望 401", recorder.Code)
		}
	})

	t.Run("items-without-tags-serialize-as-empty-array", func(t *testing.T) {
		recorder, body := env.imgTestUploadBytes(t, auth, "notags.png", imgTestPNG(t, 16, 16), nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("上传失败: status=%d", recorder.Code)
		}
		imageID := imgTestMustImageID(t, body)
		single := env.do(t, http.MethodGet, "/api/images?mine=true&query=notags", nil, "", auth)
		if !strings.Contains(single.Body.String(), `"tags":[]`) {
			t.Fatalf("无标签图片的 tags 必须是 []: %s", single.Body.String())
		}
		if !strings.Contains(single.Body.String(), imageID) {
			t.Fatalf("列表缺少新图片: %s", single.Body.String())
		}
	})
}

func TestImagesPermissionsAndTrashAccess(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	ownerCookie := env.registerAdmin(t, "owner@example.com", "password123")
	ownerAuth := imgTestCookieAuth(ownerCookie)
	_, otherCookie := env.addUser(t, "other@example.com")
	otherAuth := imgTestCookieAuth(otherCookie)
	ownerID := int64(1)
	apiKey := env.addAPIKey(t, ownerID)
	keyAuth := imgTestBearerAuth(apiKey)

	recorder, body := env.imgTestUploadBytes(t, ownerAuth, "owned.png", imgTestPNG(t, 50, 40), []string{"shared"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imageID := imgTestMustImageID(t, body)

	t.Run("cross-user-writes-rejected", func(t *testing.T) {
		payload := []byte(`{"tags":["hijacked"]}`)
		patch := env.do(t, http.MethodPatch, "/api/images/"+imageID, bytes.NewReader(payload), "application/json", otherAuth)
		if patch.Code != http.StatusForbidden {
			t.Fatalf("跨用户 PATCH status = %d，期望 403", patch.Code)
		}
		remove := env.do(t, http.MethodDelete, "/api/images/"+imageID, nil, "", otherAuth)
		if remove.Code != http.StatusForbidden {
			t.Fatalf("跨用户 DELETE status = %d，期望 403", remove.Code)
		}
		var tags int
		if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "ImageTag" WHERE imageId = ? AND name = 'hijacked'`, imageID).Scan(&tags); err != nil {
			t.Fatalf("查询标签失败: %v", err)
		}
		if tags != 0 {
			t.Fatalf("跨用户 PATCH 修改了标签")
		}
	})

	t.Run("unauthenticated-writes-rejected", func(t *testing.T) {
		patch := env.do(t, http.MethodPatch, "/api/images/"+imageID, bytes.NewReader([]byte(`{"visible":false}`)), "application/json", imgTestAuth{})
		if patch.Code != http.StatusUnauthorized {
			t.Fatalf("未登录 PATCH status = %d，期望 401", patch.Code)
		}
		remove := env.do(t, http.MethodDelete, "/api/images/"+imageID, nil, "", imgTestAuth{})
		if remove.Code != http.StatusUnauthorized {
			t.Fatalf("未登录 DELETE status = %d，期望 401", remove.Code)
		}
	})

	t.Run("public-read-allowed", func(t *testing.T) {
		public := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=original", nil, "", imgTestAuth{})
		if public.Code != http.StatusOK {
			t.Fatalf("公开读取 status = %d，期望 200", public.Code)
		}
	})

	t.Run("api-key-cannot-read-personal-list", func(t *testing.T) {
		recorder := env.do(t, http.MethodGet, "/api/images?mine=true", nil, "", keyAuth)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("API Key 读取个人列表 status = %d，期望 401", recorder.Code)
		}
	})

	t.Run("api-key-can-delete-own-image", func(t *testing.T) {
		recorder, body := env.imgTestUploadBytes(t, ownerAuth, "key-delete.png", imgTestPNG(t, 24, 16), nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		imageID := imgTestMustImageID(t, body)
		remove := env.do(t, http.MethodDelete, "/api/images/"+imageID, nil, "", keyAuth)
		if remove.Code != http.StatusOK {
			t.Fatalf("API Key 软删除 status = %d，期望 200，body=%s", remove.Code, remove.Body.String())
		}
	})

	t.Run("delete-unknown-and-already-deleted", func(t *testing.T) {
		missing := env.do(t, http.MethodDelete, "/api/images/does-not-exist", nil, "", ownerAuth)
		if missing.Code != http.StatusNotFound {
			t.Fatalf("删除不存在图片 status = %d，期望 404", missing.Code)
		}
		extraRecorder, extraBody := env.imgTestUploadBytes(t, ownerAuth, "already.png", imgTestPNG(t, 18, 12), nil)
		if extraRecorder.Code != http.StatusOK {
			t.Fatalf("上传失败: status=%d", extraRecorder.Code)
		}
		extraID := imgTestMustImageID(t, extraBody)
		imgTestTrash(t, env, ownerAuth, extraID)
		again := env.do(t, http.MethodDelete, "/api/images/"+extraID, nil, "", ownerAuth)
		if again.Code != http.StatusNotFound {
			t.Fatalf("重复删除 status = %d，期望 404", again.Code)
		}
	})

	t.Run("permanent-delete-requires-trash", func(t *testing.T) {
		// The image is still live, so a permanent DELETE must be a 409 no-op.
		recorder := env.do(t, http.MethodDelete, "/api/images/"+imageID+"?permanent=true", nil, "", ownerAuth)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("permanent DELETE status = %d，期望 409，body=%s", recorder.Code, recorder.Body.String())
		}
		if got := imgTestErrorCode(t, recorder); got != "NOT_IN_TRASH" {
			t.Fatalf("error.code = %q", got)
		}
		var deletedAt *string
		if err := env.app.db.QueryRow(`SELECT deletedAt FROM "Image" WHERE id = ?`, imageID).Scan(&deletedAt); err != nil {
			t.Fatalf("读取 deletedAt 失败: %v", err)
		}
		if deletedAt != nil {
			t.Fatalf("被拒绝的 permanent DELETE 修改了记录")
		}
	})

	t.Run("trash-permissions", func(t *testing.T) {
		imgTestTrash(t, env, ownerAuth, imageID)

		anonymous := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=original", nil, "", imgTestAuth{})
		if anonymous.Code != http.StatusNotFound {
			t.Fatalf("匿名读取已删除图片 status = %d，期望 404", anonymous.Code)
		}
		other := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=original", nil, "", otherAuth)
		if other.Code != http.StatusNotFound {
			t.Fatalf("其他用户读取已删除图片 status = %d，期望 404", other.Code)
		}
		apiKeyRead := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=original", nil, "", keyAuth)
		if apiKeyRead.Code != http.StatusNotFound {
			t.Fatalf("API Key 读取已删除图片 status = %d，期望 404（仅接受 Cookie）", apiKeyRead.Code)
		}
		ownerRead := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=original", nil, "", ownerAuth)
		if ownerRead.Code != http.StatusOK {
			t.Fatalf("上传者读取回收站图片 status = %d，期望 200，body=%s", ownerRead.Code, ownerRead.Body.String())
		}
		if cacheControl := ownerRead.Header().Get("Cache-Control"); cacheControl != "private, no-store" {
			t.Fatalf("回收站图片 Cache-Control = %q，期望 private, no-store", cacheControl)
		}
		patch := env.do(t, http.MethodPatch, "/api/images/"+imageID, bytes.NewReader([]byte(`{"visible":true}`)), "application/json", ownerAuth)
		if patch.Code != http.StatusNotFound {
			t.Fatalf("对回收站图片 PATCH status = %d，期望 404", patch.Code)
		}
		missing := env.do(t, http.MethodGet, "/api/images/does-not-exist?variant=original", nil, "", imgTestAuth{})
		if missing.Code != http.StatusNotFound {
			t.Fatalf("不存在图片 status = %d，期望 404", missing.Code)
		}
	})
}

func TestImagesConditionalRequestsAndRange(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "conditional@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	data := imgTestPNG(t, 70, 50)
	recorder, body := env.imgTestUploadBytes(t, auth, "conditional.png", data, []string{"cache"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imageID := imgTestMustImageID(t, body)
	target := "/api/images/" + imageID + "?variant=original"

	t.Run("etag-and-304", func(t *testing.T) {
		first := env.do(t, http.MethodGet, target, nil, "", imgTestAuth{})
		if first.Code != http.StatusOK {
			t.Fatalf("首次读取 status = %d", first.Code)
		}
		etag := first.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("缺少 ETag")
		}
		if cacheControl := first.Header().Get("Cache-Control"); cacheControl != "public, no-cache" {
			t.Fatalf("Cache-Control = %q，期望 public, no-cache", cacheControl)
		}
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("If-None-Match", etag)
		conditional := httptest.NewRecorder()
		env.handler.ServeHTTP(conditional, request)
		if conditional.Code != http.StatusNotModified {
			t.Fatalf("条件请求 status = %d，期望 304，body=%s", conditional.Code, conditional.Body.String())
		}
		if conditional.Body.Len() != 0 {
			t.Fatalf("304 响应不应有 body")
		}
		if conditional.Header().Get("ETag") != etag {
			t.Fatalf("304 响应缺少相同 ETag")
		}
	})

	t.Run("head", func(t *testing.T) {
		get := env.do(t, http.MethodGet, target, nil, "", imgTestAuth{})
		etag := get.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("GET 缺少 ETag")
		}

		head := env.do(t, http.MethodHead, target, nil, "", imgTestAuth{})
		if head.Code != http.StatusOK {
			t.Fatalf("HEAD status = %d", head.Code)
		}
		if head.Body.Len() != 0 {
			t.Fatalf("HEAD 不应有 body")
		}
		if contentLength := head.Header().Get("Content-Length"); contentLength != fmt.Sprintf("%d", len(data)) {
			t.Fatalf("HEAD Content-Length = %q，期望 %d", contentLength, len(data))
		}
		// GET and HEAD must expose the same validator.
		if headETag := head.Header().Get("ETag"); headETag != etag {
			t.Fatalf("HEAD ETag = %q，GET ETag = %q", headETag, etag)
		}

		// A matching validator on HEAD is 304 with no body, exactly like GET.
		request := httptest.NewRequest(http.MethodHead, target, nil)
		request.Header.Set("If-None-Match", etag)
		conditional := httptest.NewRecorder()
		env.handler.ServeHTTP(conditional, request)
		if conditional.Code != http.StatusNotModified {
			t.Fatalf("HEAD 条件请求 status = %d，期望 304，body=%s", conditional.Code, conditional.Body.String())
		}
		if conditional.Body.Len() != 0 {
			t.Fatalf("304 HEAD 不应有 body")
		}
		if conditional.Header().Get("Content-Length") != "" {
			t.Fatalf("304 HEAD 不应带 Content-Length: %q", conditional.Header().Get("Content-Length"))
		}
		if conditional.Header().Get("ETag") != etag {
			t.Fatalf("304 HEAD 缺少相同 ETag")
		}
	})

	t.Run("range", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.Header.Set("Range", "bytes=0-9")
		partial := httptest.NewRecorder()
		env.handler.ServeHTTP(partial, request)
		if partial.Code != http.StatusPartialContent {
			t.Fatalf("Range status = %d，期望 206，body=%s", partial.Code, partial.Body.String())
		}
		if !bytes.Equal(partial.Body.Bytes(), data[:10]) {
			t.Fatalf("Range 字节不匹配")
		}
		if contentRange := partial.Header().Get("Content-Range"); contentRange != fmt.Sprintf("bytes 0-9/%d", len(data)) {
			t.Fatalf("Content-Range = %q", contentRange)
		}

		suffixRequest := httptest.NewRequest(http.MethodGet, target, nil)
		suffixRequest.Header.Set("Range", "bytes=-4")
		suffix := httptest.NewRecorder()
		env.handler.ServeHTTP(suffix, suffixRequest)
		if suffix.Code != http.StatusPartialContent || !bytes.Equal(suffix.Body.Bytes(), data[len(data)-4:]) {
			t.Fatalf("后缀 Range 处理错误: status=%d", suffix.Code)
		}

		invalidRequest := httptest.NewRequest(http.MethodGet, target, nil)
		invalidRequest.Header.Set("Range", fmt.Sprintf("bytes=%d-", len(data)))
		invalid := httptest.NewRecorder()
		env.handler.ServeHTTP(invalid, invalidRequest)
		if invalid.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("越界 Range status = %d，期望 416", invalid.Code)
		}
	})

	t.Run("hidden-placeholder-never-304", func(t *testing.T) {
		first := env.do(t, http.MethodGet, target, nil, "", imgTestAuth{})
		etag := first.Header().Get("ETag")
		imgTestSetHidden(t, env, auth, imageID, false)

		hiddenRequest := httptest.NewRequest(http.MethodGet, target, nil)
		hiddenRequest.Header.Set("If-None-Match", etag)
		hidden := httptest.NewRecorder()
		env.handler.ServeHTTP(hidden, hiddenRequest)
		if hidden.Code != http.StatusOK {
			t.Fatalf("隐藏图片条件请求 status = %d，期望 200（不得 304 泄露）", hidden.Code)
		}
		if contentType := hidden.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "image/svg+xml") {
			t.Fatalf("隐藏图片 Content-Type = %q，期望 SVG 占位", contentType)
		}
		if cacheControl := hidden.Header().Get("Cache-Control"); cacheControl != "no-store" {
			t.Fatalf("隐藏占位 Cache-Control = %q，期望 no-store", cacheControl)
		}
		if etag := hidden.Header().Get("ETag"); etag != "" {
			t.Fatalf("隐藏占位不应带 ETag")
		}
		headHidden := env.do(t, http.MethodHead, target, nil, "", imgTestAuth{})
		if headHidden.Code != http.StatusOK || !strings.HasPrefix(headHidden.Header().Get("Content-Type"), "image/svg+xml") {
			t.Fatalf("隐藏占位 HEAD 错误: status=%d type=%q", headHidden.Code, headHidden.Header().Get("Content-Type"))
		}

		// Conditional HEAD on a hidden image must not become 304: the placeholder
		// is no-store and must not confirm the hidden bytes exist.
		headHiddenRequest := httptest.NewRequest(http.MethodHead, target, nil)
		headHiddenRequest.Header.Set("If-None-Match", etag)
		headHiddenConditional := httptest.NewRecorder()
		env.handler.ServeHTTP(headHiddenConditional, headHiddenRequest)
		if headHiddenConditional.Code != http.StatusOK {
			t.Fatalf("隐藏图片 HEAD 条件请求 status = %d，期望 200", headHiddenConditional.Code)
		}
		if headHiddenConditional.Body.Len() != 0 {
			t.Fatalf("隐藏占位 HEAD 不应有 body")
		}
		if !strings.HasPrefix(headHiddenConditional.Header().Get("Content-Type"), "image/svg+xml") {
			t.Fatalf("隐藏占位 HEAD Content-Type = %q", headHiddenConditional.Header().Get("Content-Type"))
		}
		if headHiddenConditional.Header().Get("ETag") != "" {
			t.Fatalf("隐藏占位 HEAD 不应带 ETag")
		}
		if cacheControl := headHiddenConditional.Header().Get("Cache-Control"); cacheControl != "no-store" {
			t.Fatalf("隐藏占位 HEAD Cache-Control = %q，期望 no-store", cacheControl)
		}
		imgTestSetHidden(t, env, auth, imageID, true)
	})

	t.Run("trashed-never-304-for-owner", func(t *testing.T) {
		first := env.do(t, http.MethodGet, target, nil, "", auth)
		etag := first.Header().Get("ETag")
		imgTestTrash(t, env, auth, imageID)

		request := httptest.NewRequest(http.MethodGet, target, nil)
		request.AddCookie(cookie)
		request.Header.Set("If-None-Match", etag)
		recorder := httptest.NewRecorder()
		env.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("回收站图片条件请求 status = %d，期望 200", recorder.Code)
		}
		if !bytes.Equal(recorder.Body.Bytes(), data) {
			t.Fatalf("回收站图片字节不匹配")
		}
		if cacheControl := recorder.Header().Get("Cache-Control"); cacheControl != "private, no-store" {
			t.Fatalf("回收站 Cache-Control = %q", cacheControl)
		}

		// Anonymous HEAD with the old validator stays a plain 404 (no existence leak).
		headAnonymousRequest := httptest.NewRequest(http.MethodHead, target, nil)
		headAnonymousRequest.Header.Set("If-None-Match", etag)
		headAnonymous := httptest.NewRecorder()
		env.handler.ServeHTTP(headAnonymous, headAnonymousRequest)
		if headAnonymous.Code != http.StatusNotFound {
			t.Fatalf("匿名 HEAD 回收站图片 status = %d，期望 404", headAnonymous.Code)
		}
		if contentType := headAnonymous.Header().Get("Content-Type"); strings.HasPrefix(contentType, "image/") {
			t.Fatalf("404 HEAD 不得返回图片内容: Content-Type=%q", contentType)
		}
		// The recorder keeps JSON error bytes; net/http suppresses HEAD bodies, so
		// emptiness is only asserted for successful responses here.

		// The owner's HEAD succeeds without a body and never 304s for private data.
		headOwnerRequest := httptest.NewRequest(http.MethodHead, target, nil)
		headOwnerRequest.AddCookie(cookie)
		headOwnerRequest.Header.Set("If-None-Match", etag)
		headOwner := httptest.NewRecorder()
		env.handler.ServeHTTP(headOwner, headOwnerRequest)
		if headOwner.Code != http.StatusOK {
			t.Fatalf("上传者 HEAD 回收站图片 status = %d，期望 200", headOwner.Code)
		}
		if headOwner.Body.Len() != 0 {
			t.Fatalf("HEAD 不应有 body")
		}
		if contentLength := headOwner.Header().Get("Content-Length"); contentLength != fmt.Sprintf("%d", len(data)) {
			t.Fatalf("回收站 HEAD Content-Length = %q，期望 %d", contentLength, len(data))
		}
		if cacheControl := headOwner.Header().Get("Cache-Control"); cacheControl != "private, no-store" {
			t.Fatalf("回收站 HEAD Cache-Control = %q，期望 private, no-store", cacheControl)
		}
	})
}

func TestImagesPatchValidation(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "patch@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	recorder, body := env.imgTestUploadBytes(t, auth, "patch.png", imgTestPNG(t, 40, 30), []string{"one", "two"})
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imageID := imgTestMustImageID(t, body)
	target := "/api/images/" + imageID

	expectError := func(t *testing.T, payload string, status int, code string) {
		t.Helper()
		recorder := env.do(t, http.MethodPatch, target, bytes.NewReader([]byte(payload)), "application/json", auth)
		if recorder.Code != status {
			t.Fatalf("status = %d，期望 %d，body=%s", recorder.Code, status, recorder.Body.String())
		}
		if got := imgTestErrorCode(t, recorder); got != code {
			t.Fatalf("error.code = %q，期望 %q", got, code)
		}
	}

	t.Run("empty-object", func(t *testing.T) { expectError(t, `{}`, http.StatusBadRequest, "INVALID_REQUEST") })
	t.Run("non-boolean-visible", func(t *testing.T) { expectError(t, `{"visible":"yes"}`, http.StatusBadRequest, "INVALID_REQUEST") })
	// json.Unmarshal(null, &bool) succeeds and yields false; the API contract
	// requires a real boolean, and accepting null would silently hide the image.
	t.Run("null-visible-has-no-side-effects", func(t *testing.T) {
		snapshot := func(t *testing.T) (isVisible int, updatedAt string) {
			t.Helper()
			if err := env.app.db.QueryRow(`SELECT isVisible, updatedAt FROM "Image" WHERE id = ?`, imageID).
				Scan(&isVisible, &updatedAt); err != nil {
				t.Fatalf("读取图片状态失败: %v", err)
			}
			return isVisible, updatedAt
		}
		tagsOf := func(t *testing.T) []string {
			t.Helper()
			rows, err := env.app.db.Query(`SELECT name FROM "ImageTag" WHERE imageId = ? ORDER BY name`, imageID)
			if err != nil {
				t.Fatalf("读取标签失败: %v", err)
			}
			defer rows.Close()
			names := make([]string, 0, 2)
			for rows.Next() {
				var name string
				if err := rows.Scan(&name); err != nil {
					t.Fatalf("扫描标签失败: %v", err)
				}
				names = append(names, name)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("遍历标签失败: %v", err)
			}
			return names
		}

		beforeVisible, beforeUpdatedAt := snapshot(t)
		beforeTags := tagsOf(t)
		if beforeVisible != 1 {
			t.Fatalf("前置状态 isVisible = %d，期望 1", beforeVisible)
		}

		expectError(t, `{"visible":null}`, http.StatusBadRequest, "INVALID_REQUEST")
		// A rejected request must not touch other fields either.
		expectError(t, `{"visible":null,"tags":["changed"]}`, http.StatusBadRequest, "INVALID_REQUEST")

		afterVisible, afterUpdatedAt := snapshot(t)
		if afterVisible != beforeVisible {
			t.Fatalf("被拒绝的 visible:null 修改了可见性: %d -> %d", beforeVisible, afterVisible)
		}
		if afterUpdatedAt != beforeUpdatedAt {
			t.Fatalf("被拒绝的 visible:null 修改了 updatedAt: %q -> %q", beforeUpdatedAt, afterUpdatedAt)
		}
		afterTags := tagsOf(t)
		if len(afterTags) != len(beforeTags) {
			t.Fatalf("被拒绝的 PATCH 修改了标签: %v -> %v", beforeTags, afterTags)
		}
		for index := range beforeTags {
			if afterTags[index] != beforeTags[index] {
				t.Fatalf("被拒绝的 PATCH 修改了标签: %v -> %v", beforeTags, afterTags)
			}
		}
	})
	t.Run("array-body", func(t *testing.T) { expectError(t, `[]`, http.StatusBadRequest, "INVALID_REQUEST") })
	t.Run("tags-not-array", func(t *testing.T) { expectError(t, `{"tags":"one"}`, http.StatusBadRequest, "INVALID_TAG") })
	t.Run("tags-null", func(t *testing.T) { expectError(t, `{"tags":null}`, http.StatusBadRequest, "INVALID_TAG") })
	t.Run("too-many-tags", func(t *testing.T) {
		tags := make([]string, 0, imagesMaxTags+1)
		for index := 0; index < imagesMaxTags+1; index++ {
			tags = append(tags, fmt.Sprintf("tag-%02d", index))
		}
		expectError(t, `{"tags":`+imgTestTagsJSON(t, tags)+`}`, http.StatusBadRequest, "INVALID_TAG")
	})
	t.Run("tag-too-long", func(t *testing.T) {
		expectError(t, `{"tags":["`+strings.Repeat("宽", imagesMaxTagLength+1)+`"]}`, http.StatusBadRequest, "INVALID_TAG")
	})

	t.Run("update-tags-and-visibility", func(t *testing.T) {
		payload := `{"visible":false,"tags":["zeta","alpha","alpha"]}`
		recorder := env.do(t, http.MethodPatch, target, bytes.NewReader([]byte(payload)), "application/json", auth)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PATCH status = %d，body=%s", recorder.Code, recorder.Body.String())
		}
		response := imgTestJSON(t, recorder)
		imageBody, ok := response["image"].(map[string]any)
		if !ok {
			t.Fatalf("响应缺少 image: %v", response)
		}
		if imageBody["isVisible"] != false {
			t.Fatalf("isVisible = %v，期望 false", imageBody["isVisible"])
		}
		tags, _ := imageBody["tags"].([]any)
		if len(tags) != 2 || tags[0] != "alpha" || tags[1] != "zeta" {
			t.Fatalf("tags 未替换/排序: %v", imageBody["tags"])
		}
		if imageBody["tag"] != "alpha" {
			t.Fatalf("tag 应为第一个排序标签: %v", imageBody["tag"])
		}
		var storedTags int
		if err := env.app.db.QueryRow(`SELECT COUNT(*) FROM "ImageTag" WHERE imageId = ?`, imageID).Scan(&storedTags); err != nil {
			t.Fatalf("统计标签失败: %v", err)
		}
		if storedTags != 2 {
			t.Fatalf("数据库标签数 = %d，期望 2", storedTags)
		}
	})

	t.Run("api-key-can-write", func(t *testing.T) {
		apiKey := env.addAPIKey(t, 1)
		recorder := env.do(t, http.MethodPatch, target, bytes.NewReader([]byte(`{"visible":true}`)), "application/json", imgTestBearerAuth(apiKey))
		if recorder.Code != http.StatusOK {
			t.Fatalf("API Key PATCH status = %d，body=%s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestImagesUnsupportedEndpoints(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "unsupported@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	cases := []struct {
		method string
		target string
		status int
		code   string
	}{
		{method: http.MethodPost, target: "/api/images/batch", status: http.StatusNotImplemented, code: "NOT_IMPLEMENTED"},
		{method: http.MethodPost, target: "/api/images/url", status: http.StatusNotImplemented, code: "NOT_IMPLEMENTED"},
		{method: http.MethodPost, target: "/api/images/export", status: http.StatusNotImplemented, code: "NOT_IMPLEMENTED"},
		// restore 只实现 POST；其他方法落到通配 handler，仍是明确的 JSON 404。
		{method: http.MethodGet, target: "/api/images/some-id/restore", status: http.StatusNotFound, code: "NOT_FOUND"},
		{method: http.MethodPost, target: "/api/images/some-id/purge", status: http.StatusNotFound, code: "NOT_FOUND"},
		{method: http.MethodGet, target: "/api/images/some-id/unknown", status: http.StatusNotFound, code: "NOT_FOUND"},
	}
	for _, testCase := range cases {
		t.Run(testCase.method+" "+testCase.target, func(t *testing.T) {
			recorder := env.do(t, testCase.method, testCase.target, nil, "", auth)
			if recorder.Code != testCase.status {
				t.Fatalf("status = %d，期望 %d，body=%s", recorder.Code, testCase.status, recorder.Body.String())
			}
			if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
				t.Fatalf("Content-Type = %q，期望 JSON", contentType)
			}
			if got := imgTestErrorCode(t, recorder); got != testCase.code {
				t.Fatalf("error.code = %q，期望 %q", got, testCase.code)
			}
		})
	}
}

func TestImagesStoredPathEscapesAreRejected(t *testing.T) {
	env := imgTestNewApp(t, Config{})
	cookie := env.registerAdmin(t, "path@example.com", "password123")
	auth := imgTestCookieAuth(cookie)

	recorder, body := env.imgTestUploadBytes(t, auth, "path.png", imgTestPNG(t, 30, 20), nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传失败: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	imageID := imgTestMustImageID(t, body)

	for _, relative := range []string{"../outside.png", "/etc/passwd", "images/../../outside.png", "images/./../../x.png"} {
		if _, err := env.app.imagesSafePath(relative); err == nil {
			t.Fatalf("不安全路径被接受: %q", relative)
		}
	}

	// A corrupted row pointing outside the data directory must 404, not serve.
	if _, err := env.app.db.Exec(`UPDATE "Image" SET originalPath = '../escape.png' WHERE id = ?`, imageID); err != nil {
		t.Fatalf("篡改存储路径失败: %v", err)
	}
	escape := env.do(t, http.MethodGet, "/api/images/"+imageID+"?variant=original", nil, "", auth)
	if escape.Code != http.StatusNotFound {
		t.Fatalf("越界存储路径 status = %d，期望 404", escape.Code)
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
