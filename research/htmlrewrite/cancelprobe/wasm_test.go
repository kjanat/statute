package cancelprobe

// This small encoder keeps the reproducer independent of Rust, a WAT compiler,
// HTML, linear memory, and allocator behavior. See README.md for equivalent WAT.
func fixture(batch int) []byte {
	if batch != 1 && batch != 16 && batch != 256 {
		panic("unsupported fixture batch")
	}
	module := []byte{0, 97, 115, 109, 1, 0, 0, 0}
	section := func(id byte, data []byte) {
		module = append(module, id)
		module = append(module, uleb(uint32(len(data)))...)
		module = append(module, data...)
	}
	// Types: (i32, i32) -> i32, () -> (). The sole import is probe.ready.
	section(1, []byte{2, 0x60, 2, 0x7f, 0x7f, 1, 0x7f, 0x60, 0, 0})
	section(2, []byte{1, 5, 'p', 'r', 'o', 'b', 'e', 5, 'r', 'e', 'a', 'd', 'y', 0, 1})
	section(3, []byte{2, 0, 1})
	// One instance-owned mutable i32 global, initially 1, used by spin.
	section(6, []byte{1, 0x7f, 1, 0x41, 1, 0x0b})
	section(7, []byte{3, 4, 'w', 'o', 'r', 'k', 0, 1, 4, 's', 'p', 'i', 'n', 0, 2, 5, 's', 't', 'a', 't', 'e', 3, 0})
	// work(groups, seed): guard zero, then batch xorshift steps per loop.
	work := []byte{0, 0x02, 0x40, 0x20, 0, 0x45, 0x0d, 0, 0x03, 0x40}
	work = append(work, steps(batch, 1)...)
	work = append(work, 0x20, 0, 0x41, 1, 0x6b, 0x22, 0, 0x0d, 0, 0x0b, 0x0b, 0x20, 1, 0x0b)
	// spin: warm up one batch, notify the host once, then work forever.
	spin := []byte{1, 1, 0x7f, 0x23, 0, 0x21, 0}
	spin = append(spin, steps(batch, 0)...)
	spin = append(spin, 0x20, 0, 0x24, 0, 0x10, 0, 0x03, 0x40)
	spin = append(spin, steps(batch, 0)...)
	spin = append(spin, 0x20, 0, 0x24, 0, 0x0c, 0, 0x0b, 0x0b)
	code := []byte{2}
	for _, body := range [][]byte{work, spin} {
		code = append(code, uleb(uint32(len(body)))...)
		code = append(code, body...)
	}
	section(10, code)
	return module
}

func uleb(n uint32) []byte {
	var encoded []byte
	for {
		b := byte(n & 127)
		n >>= 7
		if n != 0 {
			b |= 128
		}
		encoded = append(encoded, b)
		if n == 0 {
			return encoded
		}
	}
}

func steps(batch int, local byte) []byte {
	var code []byte
	for range batch {
		for _, shift := range []struct{ amount, opcode byte }{{13, 0x74}, {17, 0x76}, {5, 0x74}} {
			// local.get x; local.get x; i32.const shift; shl/shr_u;
			// i32.xor; local.set x.
			code = append(code, 0x20, local, 0x20, local, 0x41, shift.amount, shift.opcode, 0x73, 0x21, local)
		}
	}
	return code
}

func reference(n int, seed uint32) uint32 {
	for range n {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
	}
	return seed
}
