// 将 logo.jpg 转换为 Windows 图标 logo.ico（PNG 压缩格式，Vista+ 支持）
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	_ "image/png"
	"os"
)

// 双线性插值缩放
func resize(src image.Image, size int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	sb := src.Bounds()
	sw, sh := float64(sb.Dx()), float64(sb.Dy())
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx := (float64(x)+0.5)*sw/float64(size) - 0.5
			fy := (float64(y)+0.5)*sh/float64(size) - 0.5
			x0, y0 := int(fx), int(fy)
			if x0 < 0 {
				x0 = 0
			}
			if y0 < 0 {
				y0 = 0
			}
			x1, y1 := x0+1, y0+1
			if x1 >= sb.Dx() {
				x1 = sb.Dx() - 1
			}
			if y1 >= sb.Dy() {
				y1 = sb.Dy() - 1
			}
			dx, dy := fx-float64(x0), fy-float64(y0)
			if dx < 0 {
				dx = 0
			}
			if dy < 0 {
				dy = 0
			}
			c00 := src.At(sb.Min.X+x0, sb.Min.Y+y0)
			c10 := src.At(sb.Min.X+x1, sb.Min.Y+y0)
			c01 := src.At(sb.Min.X+x0, sb.Min.Y+y1)
			c11 := src.At(sb.Min.X+x1, sb.Min.Y+y1)
			r00, g00, b00, a00 := c00.RGBA()
			r10, g10, b10, a10 := c10.RGBA()
			r01, g01, b01, a01 := c01.RGBA()
			r11, g11, b11, a11 := c11.RGBA()
			lerp := func(v00, v10, v01, v11 uint32) uint8 {
				top := float64(v00)*(1-dx) + float64(v10)*dx
				bot := float64(v01)*(1-dx) + float64(v11)*dx
				return uint8((top*(1-dy) + bot*dy) / 257)
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = lerp(r00, r10, r01, r11)
			dst.Pix[i+1] = lerp(g00, g10, g01, g11)
			dst.Pix[i+2] = lerp(b00, b10, b01, b11)
			dst.Pix[i+3] = lerp(a00, a10, a01, a11)
		}
	}
	return dst
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("用法: mkico <输入.jpg> <输出.ico>")
		os.Exit(1)
	}
	f, err := os.Open(os.Args[1])
	if err != nil {
		fmt.Println("打开输入失败:", err)
		os.Exit(1)
	}
	defer f.Close()
	src, err := jpeg.Decode(f)
	if err != nil {
		fmt.Println("解码 JPG 失败:", err)
		os.Exit(1)
	}

	img := resize(src, 256)
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		fmt.Println("编码 PNG 失败:", err)
		os.Exit(1)
	}
	pngData := pngBuf.Bytes()

	out, err := os.Create(os.Args[2])
	if err != nil {
		fmt.Println("创建输出失败:", err)
		os.Exit(1)
	}
	defer out.Close()

	// ICONDIR: reserved(2) type(2)=1 count(2)=1
	_ = binary.Write(out, binary.LittleEndian, uint16(0))
	_ = binary.Write(out, binary.LittleEndian, uint16(1))
	_ = binary.Write(out, binary.LittleEndian, uint16(1))
	// ICONDIRENTRY: 宽高 256 用 0 表示
	entry := []byte{0, 0, 0, 0}
	entry = append(entry, 1, 0)          // planes
	entry = append(entry, 32, 0)         // bitcount
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], uint32(len(pngData)))
	entry = append(entry, sz[:]...)
	binary.LittleEndian.PutUint32(sz[:], 22) // 图像数据偏移 = 6 + 16
	entry = append(entry, sz[:]...)
	out.Write(entry)
	out.Write(pngData)
	fmt.Println("生成成功:", os.Args[2], len(pngData), "bytes PNG 数据")
}
