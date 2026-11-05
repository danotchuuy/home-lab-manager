// Package qr makes QR codes (byte mode, error correction level M, versions
// 1 to 10: up to 213 bytes), so an otpauth:// URL can be shown to an
// authenticator app without sending its secret anywhere.
package qr

import (
	"errors"
	"fmt"
	"strings"
)

// Per version (index 1-10), at level M.
var (
	eccPerBlock = [...]int{0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26}
	numBlocks   = [...]int{0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5}
)

const maxVersion = 10

// Code is a QR code's modules, true for dark.
type Code struct {
	Size    int
	modules [][]bool
	fn      [][]bool // function patterns, which masks leave alone
}

// Dark reports whether the module at x, y is dark.
func (c *Code) Dark(x, y int) bool { return c.modules[y][x] }

// ErrTooLong is returned for data that doesn't fit version 10.
var ErrTooLong = errors.New("too much data for a QR code here")

// Encode makes the smallest QR code holding data.
func Encode(data []byte) (*Code, error) {
	for v := 1; v <= maxVersion; v++ {
		if capacity(v) >= len(data) {
			return encode(data, v), nil
		}
	}
	return nil, ErrTooLong
}

func rawModules(v int) int {
	n := (16*v+128)*v + 64
	if v >= 2 {
		align := v/7 + 2
		n -= (25*align-10)*align - 55
		if v >= 7 {
			n -= 36
		}
	}
	return n
}

func dataCodewords(v int) int { return rawModules(v)/8 - eccPerBlock[v]*numBlocks[v] }

// capacity is how many bytes version v holds: 4 bits of mode and 8 (or 16)
// of length come first.
func capacity(v int) int {
	countBits := 8
	if v >= 10 {
		countBits = 16
	}
	return (dataCodewords(v)*8 - 4 - countBits) / 8
}

func encode(data []byte, v int) *Code {
	// The bit stream: byte mode, length, data, terminator, padding.
	var bits []bool
	put := func(val, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, val>>i&1 == 1)
		}
	}
	put(4, 4)
	if v >= 10 {
		put(len(data), 16)
	} else {
		put(len(data), 8)
	}
	for _, b := range data {
		put(int(b), 8)
	}
	total := dataCodewords(v) * 8
	put(0, min(4, total-len(bits)))
	put(0, (8-len(bits)%8)%8)
	for pad := 0xEC; len(bits) < total; pad ^= 0xEC ^ 0x11 {
		put(pad, 8)
	}
	cw := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			cw[i/8] |= 1 << (7 - i%8)
		}
	}

	c := &Code{Size: v*4 + 17}
	c.modules, c.fn = grid(c.Size), grid(c.Size)
	c.drawFunctions(v)
	c.drawCodewords(interleave(cw, v))
	best, bestPenalty := 0, -1
	for m := 0; m < 8; m++ {
		c.mask(m)
		c.drawFormat(m)
		if p := c.penalty(); bestPenalty < 0 || p < bestPenalty {
			best, bestPenalty = m, p
		}
		c.mask(m) // undo
	}
	c.mask(best)
	c.drawFormat(best)
	return c
}

func grid(n int) [][]bool {
	g := make([][]bool, n)
	for i := range g {
		g[i] = make([]bool, n)
	}
	return g
}

func (c *Code) set(x, y int, dark bool) {
	c.modules[y][x] = dark
	c.fn[y][x] = true
}

func (c *Code) drawFunctions(v int) {
	n := c.Size
	for i := 0; i < n; i++ {
		c.set(6, i, i%2 == 0)
		c.set(i, 6, i%2 == 0)
	}
	for _, p := range [][2]int{{3, 3}, {n - 4, 3}, {3, n - 4}} {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := p[0]+dx, p[1]+dy
				if x >= 0 && x < n && y >= 0 && y < n {
					d := max(abs(dx), abs(dy))
					c.set(x, y, d != 2 && d != 4)
				}
			}
		}
	}
	pos := alignment(v)
	for i, ax := range pos {
		for j, ay := range pos {
			last := len(pos) - 1
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					c.set(ax+dx, ay+dy, max(abs(dx), abs(dy)) != 1)
				}
			}
		}
	}
	c.drawFormat(0) // reserve the format areas
	if v >= 7 {
		rem := v
		for i := 0; i < 12; i++ {
			rem = rem<<1 ^ (rem>>11)*0x1F25
		}
		bits := v<<12 | rem
		for i := 0; i < 18; i++ {
			dark := bits>>i&1 == 1
			a, b := n-11+i%3, i/3
			c.set(a, b, dark)
			c.set(b, a, dark)
		}
	}
}

func alignment(v int) []int {
	if v == 1 {
		return nil
	}
	num := v/7 + 2
	step := (v*8 + num*3 + 5) / (num*4 - 4) * 2
	out := make([]int, num)
	out[0] = 6
	for i, p := num-1, v*4+17-7; i >= 1; i, p = i-1, p-step {
		out[i] = p
	}
	return out
}

// drawFormat draws the format bits for level M and the mask.
func (c *Code) drawFormat(mask int) {
	data := 0<<3 | mask // M is 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = rem<<1 ^ (rem>>9)*0x537
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return bits>>i&1 == 1 }
	n := c.Size
	for i := 0; i <= 5; i++ {
		c.set(8, i, bit(i))
	}
	c.set(8, 7, bit(6))
	c.set(8, 8, bit(7))
	c.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		c.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		c.set(n-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		c.set(8, n-15+i, bit(i))
	}
	c.set(8, n-8, true)
}

// interleave adds the error correction and interleaves the blocks.
func interleave(data []byte, v int) []byte {
	blocks, ecc := numBlocks[v], eccPerBlock[v]
	raw := rawModules(v) / 8
	short := blocks - raw%blocks
	shortLen := raw / blocks
	gen := generator(ecc)
	var datas, eccs [][]byte
	k := 0
	for i := 0; i < blocks; i++ {
		n := shortLen - ecc
		if i >= short {
			n++
		}
		d := data[k : k+n]
		k += n
		datas = append(datas, d)
		eccs = append(eccs, remainder(d, gen))
	}
	var out []byte
	for i := 0; i <= shortLen-ecc; i++ {
		for j, d := range datas {
			if i < shortLen-ecc || j >= short {
				out = append(out, d[i])
			}
		}
	}
	for i := 0; i < ecc; i++ {
		for _, e := range eccs {
			out = append(out, e[i])
		}
	}
	return out
}

func gfMul(x, y byte) byte {
	var z byte
	for i := 7; i >= 0; i-- {
		hi := z >> 7
		z = z<<1 ^ hi*0x1D
		z ^= (y >> i & 1) * x
	}
	return z
}

func generator(degree int) []byte {
	res := make([]byte, degree)
	res[degree-1] = 1
	root := byte(1)
	for i := 0; i < degree; i++ {
		for j := range res {
			res[j] = gfMul(res[j], root)
			if j+1 < len(res) {
				res[j] ^= res[j+1]
			}
		}
		root = gfMul(root, 2)
	}
	return res
}

func remainder(data, gen []byte) []byte {
	res := make([]byte, len(gen))
	for _, b := range data {
		f := b ^ res[0]
		copy(res, res[1:])
		res[len(res)-1] = 0
		for i, g := range gen {
			res[i] ^= gfMul(g, f)
		}
	}
	return res
}

func (c *Code) drawCodewords(data []byte) {
	n := c.Size
	i := 0
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = n - 1 - vert
				}
				if !c.fn[y][x] && i < len(data)*8 {
					c.modules[y][x] = data[i>>3]>>(7-i&7)&1 == 1
					i++
				}
			}
		}
	}
}

func (c *Code) mask(m int) {
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			default:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv && !c.fn[y][x] {
				c.modules[y][x] = !c.modules[y][x]
			}
		}
	}
}

// penalty scores how hard a masked code is to read: runs of one colour, 2x2
// blocks, and an uneven balance of dark and light.
func (c *Code) penalty() int {
	n, p, dark := c.Size, 0, 0
	for i := 0; i < n; i++ {
		for _, row := range []bool{true, false} {
			run, last := 0, false
			for j := 0; j < n; j++ {
				v := c.modules[i][j]
				if !row {
					v = c.modules[j][i]
				}
				if j > 0 && v == last {
					run++
				} else {
					run = 1
				}
				if run == 5 {
					p += 3
				} else if run > 5 {
					p++
				}
				last = v
			}
		}
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			v := c.modules[y][x]
			if v {
				dark++
			}
			if x < n-1 && y < n-1 && v == c.modules[y][x+1] && v == c.modules[y+1][x] && v == c.modules[y+1][x+1] {
				p += 3
			}
		}
	}
	k := abs(dark*20-n*n*10)/(n*n) - 1
	if k > 0 {
		p += k * 10
	}
	return p
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// SVG draws the code, with a quiet zone of 4 modules, scaled to fit the
// viewBox (one unit per module).
func (c *Code) SVG() string {
	n := c.Size + 8
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.modules[y][x] {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+4, y+4)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}
