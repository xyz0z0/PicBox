package server

// 动画 WebP 的首帧解码。
//
// 纯 Go 的 golang.org/x/image/webp 只支持静态 WebP（遇到 'ANMF' 会以 invalid
// format 失败），这里按 RFC 9649 解析容器：读取 'VP8X' 画布，逐帧校验 'ANMF'
// 边界与位流头，只把首帧的 'ALPH'/'VP8 '/'VP8L' 子块通过 io.MultiReader 拼成
// 静态 WebP 流交给 x/image/webp 解码。这样复用了已审计的 VP8/VP8L/ALPH 解码器，
// 不需要 CGO 或外部工具；后续帧只做轻量校验，不解码像素。
//
// 缩略图语义对齐 PicHome 的 sharp：数据库宽高与缩略图画布取 'VP8X' 画布尺寸，
// 首帧按 (2 * Frame X, 2 * Frame Y) 画到透明画布上。sharp/libwebp 读取首帧时
// 画布初值是透明黑，'ANIM' 背景色只在帧被 dispose 之后才参与，因此这里不填
// 背景色；首帧之外未覆盖的区域保持透明。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	stddraw "image/draw"
	"io"
	"os"
	"slices"

	"golang.org/x/image/riff"
	"golang.org/x/image/vp8"
	"golang.org/x/image/vp8l"
	"golang.org/x/image/webp"
)

var (
	webpFCCALPH = riff.FourCC{'A', 'L', 'P', 'H'}
	webpFCCANIM = riff.FourCC{'A', 'N', 'I', 'M'}
	webpFCCANMF = riff.FourCC{'A', 'N', 'M', 'F'}
	webpFCCVP8  = riff.FourCC{'V', 'P', '8', ' '}
	webpFCCVP8L = riff.FourCC{'V', 'P', '8', 'L'}
	webpFCCVP8X = riff.FourCC{'V', 'P', '8', 'X'}
	webpFCCWEBP = riff.FourCC{'W', 'E', 'B', 'P'}
)

const (
	// webpVP8XAnimationBit 是 'VP8X' 标志字节里的 Animation 位（RFC 9649 §2.7）。
	webpVP8XAnimationBit = 1 << 1
	// webpVP8XAlphaBit 是 'VP8X' 标志字节里的 Alpha 位。
	webpVP8XAlphaBit = 1 << 4
	// webpANMFFrameHeaderSize 是 'ANMF' 载荷里帧参数的长度。
	webpANMFFrameHeaderSize = 16
	// webpChunkHeaderSize 是 RIFF chunk 头的长度（FourCC + uint32 长度）。
	webpChunkHeaderSize = 8
	// webpRIFFHeaderSize 是 'RIFF' + 长度 + 'WEBP' 的长度。
	webpRIFFHeaderSize = 12
)

var errInvalidAnimatedWebP = errors.New("webp: invalid animated image")

// imagesWebPFrameData 是一帧经过结构校验的 'ANMF' 数据。只有首帧会保留
// payload/alpha/bitstream；后续帧只保留解析出的帧参数用于边界校验。
type imagesWebPFrameData struct {
	x, y, width, height int
	alpha               []byte
	payload             []byte
}

// imagesWebPAnimated reports whether the WebP container advertises animation
// (VP8X chunk with the animation flag). Such files need the animated first-frame
// path; the plain decoder only reads still images.
func imagesWebPAnimated(head []byte) bool {
	if len(head) < 21 || !bytes.Equal(head[:4], []byte("RIFF")) || !bytes.Equal(head[8:12], []byte("WEBP")) {
		return false
	}
	if !bytes.Equal(head[12:16], []byte("VP8X")) {
		return false
	}
	return head[20]&webpVP8XAnimationBit != 0
}

// imagesWebPAnimatedFirstFrame decodes only the first frame of an animated WebP
// and composites it onto a transparent canvas. Every dimension is validated
// before the corresponding allocation:
//   - the RIFF header's declared end may not exceed the real file size, so a
//     truncated tail (for example a cut-off second frame) is rejected;
//   - the canvas is checked against maxPixels before the canvas is allocated;
//   - the frame rectangle must fit inside the canvas, so the frame decoder can
//     never allocate more than the validated canvas;
//   - later frames are walked structurally and their VP8/VP8L headers are checked
//     without decoding pixels, matching the formats sharp/libwebp accept.
func imagesWebPAnimatedFirstFrame(filePath string, maxPixels int64) (image.Image, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < webpRIFFHeaderSize {
		return nil, errInvalidAnimatedWebP
	}

	var header [webpRIFFHeaderSize]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, err
	}
	if !bytes.Equal(header[0:4], []byte("RIFF")) || !bytes.Equal(header[8:12], []byte("WEBP")) {
		return nil, errInvalidAnimatedWebP
	}
	// RIFF 长度不含前 8 字节；声明末端超出实际文件说明内容被截断。
	declaredEnd := int64(binary.LittleEndian.Uint32(header[4:8])) + 8
	if declaredEnd < webpRIFFHeaderSize || declaredEnd > info.Size() {
		return nil, errInvalidAnimatedWebP
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	formType, reader, err := riff.NewReader(io.LimitReader(file, declaredEnd))
	if err != nil {
		return nil, err
	}
	if formType != webpFCCWEBP {
		return nil, errInvalidAnimatedWebP
	}

	canvasWidth, canvasHeight := 0, 0
	seenVP8X, seenANIM := false, false
	var firstFrame *imagesWebPFrameData
	for {
		chunkID, chunkLen, chunkData, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch chunkID {
		case webpFCCVP8X:
			if seenVP8X || chunkLen != 10 {
				return nil, errInvalidAnimatedWebP
			}
			seenVP8X = true
			var payload [10]byte
			if _, err := io.ReadFull(chunkData, payload[:]); err != nil {
				return nil, err
			}
			if payload[0]&webpVP8XAnimationBit == 0 {
				return nil, errInvalidAnimatedWebP
			}
			canvasWidth = 1 + imagesWebPReadUint24(payload[4:7])
			canvasHeight = 1 + imagesWebPReadUint24(payload[7:10])
			if err := imagesCheckDimensions(canvasWidth, canvasHeight, maxPixels); err != nil {
				return nil, err
			}
		case webpFCCANIM:
			// 动画必须带 6 字节的 'ANIM'（背景色 + 循环次数），且位于首帧之前。
			if !seenVP8X || seenANIM || chunkLen != 6 {
				return nil, errInvalidAnimatedWebP
			}
			seenANIM = true
		case webpFCCANMF:
			if !seenVP8X || !seenANIM {
				return nil, errInvalidAnimatedWebP
			}
			// 首帧需要完整载荷来解码像素；后续帧流式校验后丢弃。
			capture := firstFrame == nil
			frame, err := imagesWebPReadFrame(chunkData, chunkLen, capture)
			if err != nil {
				return nil, err
			}
			if int64(frame.x)+int64(frame.width) > int64(canvasWidth) ||
				int64(frame.y)+int64(frame.height) > int64(canvasHeight) {
				return nil, errInvalidAnimatedWebP
			}
			if capture {
				firstFrame = &frame
			}
		}
	}
	if firstFrame == nil {
		// 动画位被置位却没有任何帧：原图不完整。
		return nil, errInvalidAnimatedWebP
	}
	return imagesWebPCompositeFirstFrame(*firstFrame, canvasWidth, canvasHeight)
}

// imagesWebPReadFrame validates one 'ANMF' payload and optionally keeps its bytes.
// The payload is bounded by chunkLen (already capped by the RIFF size) and by the
// real file, and each subchunk is bounds-checked before it is read or converted
// to int. The bitstream header is validated for every frame; pixel data is only
// decoded later, for the first frame.
func imagesWebPReadFrame(r io.Reader, payloadLen uint32, capture bool) (imagesWebPFrameData, error) {
	var frame imagesWebPFrameData
	if payloadLen < webpANMFFrameHeaderSize {
		return frame, errInvalidAnimatedWebP
	}
	maxInt := uint64(^uint(0) >> 1)
	if uint64(payloadLen) > maxInt {
		return frame, errInvalidAnimatedWebP
	}
	var header [webpANMFFrameHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame, err
	}
	// Frame X/Y 在文件里存的是实际坐标的一半（RFC 9649 §2.7.1.1）。
	frame.x = 2 * imagesWebPReadUint24(header[0:3])
	frame.y = 2 * imagesWebPReadUint24(header[3:6])
	frame.width = 1 + imagesWebPReadUint24(header[6:9])
	frame.height = 1 + imagesWebPReadUint24(header[9:12])

	var payload []byte
	if capture {
		payload = make([]byte, 0, int(payloadLen))
		payload = append(payload, header[:]...)
	}

	remaining := int64(payloadLen) - webpANMFFrameHeaderSize
	alphaSeen, bitstreamSeen := false, false
	alphaOffset, alphaSize := 0, 0
	for remaining > 0 {
		if remaining < webpChunkHeaderSize {
			return frame, errInvalidAnimatedWebP
		}
		var chunkHeader [webpChunkHeaderSize]byte
		if _, err := io.ReadFull(r, chunkHeader[:]); err != nil {
			return frame, err
		}
		remaining -= webpChunkHeaderSize
		chunkID := riff.FourCC{chunkHeader[0], chunkHeader[1], chunkHeader[2], chunkHeader[3]}
		chunkSize := binary.LittleEndian.Uint32(chunkHeader[4:8])
		// 先按 uint32 与剩余字节比较，确认能放进载荷后再转 int。
		if uint64(chunkSize) > uint64(remaining) || uint64(chunkSize) > maxInt {
			return frame, errInvalidAnimatedWebP
		}
		remaining -= int64(chunkSize)

		var dataReader io.Reader
		dataOffset := 0
		if capture {
			// 直接读进唯一一份载荷缓冲，避免逐块再复制一次。
			dataOffset = len(payload) + webpChunkHeaderSize
			payload = append(payload, chunkHeader[:]...)
			payload = slices.Grow(payload, int(chunkSize))
			payload = payload[:dataOffset+int(chunkSize)]
			if _, err := io.ReadFull(r, payload[dataOffset:]); err != nil {
				return frame, err
			}
			dataReader = bytes.NewReader(payload[dataOffset:])
		} else {
			dataReader = io.LimitReader(r, int64(chunkSize))
		}

		switch chunkID {
		case webpFCCALPH:
			if alphaSeen || bitstreamSeen {
				return frame, errInvalidAnimatedWebP
			}
			alphaSeen = true
			if err := imagesWebPValidateAlphaHeader(dataReader, chunkSize, frame.width, frame.height); err != nil {
				return frame, err
			}
			alphaOffset, alphaSize = dataOffset, int(chunkSize)
		case webpFCCVP8, webpFCCVP8L:
			if bitstreamSeen || (chunkID == webpFCCVP8L && alphaSeen) {
				return frame, errInvalidAnimatedWebP
			}
			bitstreamSeen = true
			if err := imagesWebPValidateFrameBitstream(dataReader, chunkID, chunkSize, frame.width, frame.height); err != nil {
				return frame, err
			}
		}
		if !capture {
			// 排空该子块的剩余数据，riff 只会在下一次 Next 时跳过未读部分。
			if _, err := io.Copy(io.Discard, dataReader); err != nil {
				return frame, err
			}
		}
		if chunkSize%2 == 1 {
			// 每个子块都按 RIFF 规则补到偶数长度；补齐字节必须在 ANMF 载荷内。
			if remaining < 1 {
				return frame, errInvalidAnimatedWebP
			}
			remaining--
			var pad [1]byte
			if _, err := io.ReadFull(r, pad[:]); err != nil {
				return frame, err
			}
			if capture {
				payload = append(payload, 0)
			}
		}
	}
	if !bitstreamSeen {
		return frame, errInvalidAnimatedWebP
	}
	if capture {
		frame.payload = payload
		if alphaSize > 0 {
			frame.alpha = payload[alphaOffset : alphaOffset+alphaSize]
		}
	}
	return frame, nil
}

// imagesWebPValidateAlphaHeader checks the 1-byte 'ALPH' header and the cheap
// size bound for uncompressed alpha. Compressed alpha is fully validated only
// when the frame is actually decoded (the first frame).
func imagesWebPValidateAlphaHeader(r io.Reader, chunkSize uint32, width, height int) error {
	if chunkSize < 1 {
		return errInvalidAnimatedWebP
	}
	var header [1]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	switch header[0] & 0x03 {
	case 0:
		// Compression 0: one raw alpha byte per pixel.
		if int64(chunkSize)-1 < int64(width)*int64(height) {
			return errInvalidAnimatedWebP
		}
	case 1:
		// Compression 1: VP8L-compressed alpha; needs at least one data byte.
		if chunkSize < 2 {
			return errInvalidAnimatedWebP
		}
	default:
		return errInvalidAnimatedWebP
	}
	return nil
}

// imagesWebPValidateFrameBitstream reads only the VP8/VP8L header and checks the
// dimensions against the ANMF frame rectangle, without decoding pixels.
func imagesWebPValidateFrameBitstream(r io.Reader, chunkID riff.FourCC, chunkSize uint32, width, height int) error {
	if chunkSize == 0 {
		return errInvalidAnimatedWebP
	}
	switch chunkID {
	case webpFCCVP8:
		if uint64(chunkSize) > uint64(^uint(0)>>1) {
			return errInvalidAnimatedWebP
		}
		decoder := vp8.NewDecoder()
		decoder.Init(r, int(chunkSize))
		frameHeader, err := decoder.DecodeFrameHeader()
		if err != nil {
			return err
		}
		if frameHeader.Width != width || frameHeader.Height != height {
			return errInvalidAnimatedWebP
		}
	case webpFCCVP8L:
		config, err := vp8l.DecodeConfig(r)
		if err != nil {
			return err
		}
		if config.Width != width || config.Height != height {
			return errInvalidAnimatedWebP
		}
	default:
		return errInvalidAnimatedWebP
	}
	return nil
}

// imagesWebPCompositeFirstFrame rebuilds the first frame as a still WebP stream
// and draws it at its canvas offset.
func imagesWebPCompositeFirstFrame(frame imagesWebPFrameData, canvasWidth, canvasHeight int) (image.Image, error) {
	frameReader, err := imagesWebPFrameReader(frame.width, frame.height, frame.alpha != nil, frame.payload[webpANMFFrameHeaderSize:])
	if err != nil {
		return nil, err
	}
	decoded, err := webp.Decode(frameReader)
	if err != nil {
		return nil, err
	}
	bounds := decoded.Bounds()
	if bounds.Dx() != frame.width || bounds.Dy() != frame.height || bounds.Min != (image.Point{}) {
		// x/image 会核对 VP8X 与位流尺寸，这里再断言一次缩略图缩放的前提。
		return nil, errInvalidAnimatedWebP
	}

	if frame.x == 0 && frame.y == 0 && frame.width == canvasWidth && frame.height == canvasHeight {
		// 首帧铺满画布：解码结果本身就是合成后的画布，省掉一份全画布拷贝。
		return decoded, nil
	}
	canvas := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	rect := image.Rect(frame.x, frame.y, frame.x+frame.width, frame.y+frame.height)
	// 首帧叠在透明画布上：B=0（alpha 混合）与 B=1（直接覆盖）结果相同，
	// 都是把帧像素原样放到矩形内，矩形外保持透明。
	stddraw.Draw(canvas, rect, decoded, bounds.Min, stddraw.Src)
	return canvas, nil
}

// imagesWebPFrameReader streams one frame as a minimal still WebP: a fixed
// RIFF/WEBP/VP8X header followed by the frame's original subchunks (including
// their chunk headers and padding). The 'VP8X' header carries the frame
// dimensions, so the decoder compares them against the inner VP8/VP8L header
// before allocating and rejects bitstreams that lie about their size. Unknown
// subchunks are skipped by the decoder, as RFC 9649 requires. Nothing is copied
// beyond the 30-byte header; the frame payload is referenced in place.
func imagesWebPFrameReader(width, height int, hasAlpha bool, subchunks []byte) (io.Reader, error) {
	const headerSize = webpRIFFHeaderSize + webpChunkHeaderSize + 10
	// RIFF 长度 = 'WEBP' + 'VP8X' chunk + 帧子块。
	const fixedContentSize = 4 + webpChunkHeaderSize + 10
	// RIFF 长度字段是 uint32：帧子块加上固定头必须可表示。
	if uint64(len(subchunks))+fixedContentSize > 0xFFFFFFFF {
		return nil, errInvalidAnimatedWebP
	}
	header := make([]byte, headerSize)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(fixedContentSize+len(subchunks)))
	copy(header[8:12], "WEBP")
	copy(header[12:16], "VP8X")
	binary.LittleEndian.PutUint32(header[16:20], 10)
	if hasAlpha {
		header[20] = webpVP8XAlphaBit
	}
	imagesWebPWriteUint24(header[24:27], width-1)
	imagesWebPWriteUint24(header[27:30], height-1)
	return io.MultiReader(bytes.NewReader(header), bytes.NewReader(subchunks)), nil
}

func imagesWebPReadUint24(b []byte) int {
	return int(b[0]) | int(b[1])<<8 | int(b[2])<<16
}

func imagesWebPWriteUint24(b []byte, value int) {
	b[0] = byte(value)
	b[1] = byte(value >> 8)
	b[2] = byte(value >> 16)
}
