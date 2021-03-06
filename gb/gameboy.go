package gb

import (
	"fmt"
	"github.com/Humpheh/goboy/bits"
	"bufio"
	"log"
)

const (
	ClockSpeed   = 4194304
	FramesSecond = 60
	CyclesFrame  = ClockSpeed / FramesSecond

	TIMA = 0xFF05
	TMA  = 0xFF06
	TMC  = 0xFF07
)

type Gameboy struct {
	Memory          *Memory
	CPU             *CPU
	Sound           *Sound
	TimerCounter    int
	ScanlineCounter int

	ScreenData   [160][144][3]uint8
	PreparedData [160][144][3]uint8
	// Track colour of tiles in scanline
	TileScanline [160]uint8

	InterruptsEnabling bool
	InterruptsOn       bool
	Halted             bool

	CBInst    map[byte]func()
	InputMask byte
	// Callback when the serial port is written to
	TransferFunction func(byte)
	Debug            DebugFlags
	EnableSound      bool
	ExecutionPaused  bool

	CGBMode bool
	BGPalette *CGBPalette
	SpritePalette *CGBPalette

	CurrentSpeed byte
	PrepareSpeed bool

	thisCpuTicks int
	DebugScanner *bufio.Scanner
}

// Should be called 60 times/second
func (gb *Gameboy) Update() int {
	if gb.ExecutionPaused {
		return 0
	}

	cycles := 0
	for cycles < CyclesFrame * gb.getSpeed() {
		cycles_op := 4
		if !gb.Halted {
			if gb.Debug.OutputOpcodes {
				logOpcode(gb)
				fmt.Println(cpuStateString(gb.CPU, ""))
			}
			cycles_op = gb.ExecuteNextOpcode()
			cycles += cycles_op
		} else {
			// TODO: This is incorrect
			cycles += 4
		}
		if gb.IsCGB() {
			gb.checkSpeedSwitch()
		}
		gb.updateTimers(cycles_op)
		gb.updateGraphics(cycles_op)
		gb.doInterrupts()
	}
	gb.Sound.Tick(cycles)

	return cycles
}

// Get the current CPU speed multiplier (either 1 or 2).
func (gb *Gameboy) getSpeed() int {
	return int(gb.CurrentSpeed + 1)
}

// Check if the speed needs to be switched for CGB mode.
func (gb *Gameboy) checkSpeedSwitch() {
	// TODO: This should actually happen after a STOP after asking to switch
	if gb.PrepareSpeed {
		// Switch speed
		gb.PrepareSpeed = false
		if gb.CurrentSpeed == 0 {
			gb.CurrentSpeed = 1
		} else {
			gb.CurrentSpeed = 0
		}
		log.Print("new speed", gb.CurrentSpeed)
		gb.Halted = false
	}
}

func (gb *Gameboy) updateTimers(cycles int) {
	gb.dividerRegister(cycles)

	if gb.isClockEnabled() {
		gb.TimerCounter -= cycles

		if gb.TimerCounter <= 0 {
			gb.TimerCounter += gb.getClockFreqCount()

			if gb.Memory.Read(TIMA) == 255 {
				gb.Memory.Write(TIMA, gb.Memory.Read(TMA))
				gb.RequestInterrupt(2)
			} else {
				gb.Memory.Write(TIMA, gb.Memory.Read(TIMA)+1)
			}
		}
	}
}

func (gb *Gameboy) isClockEnabled() bool {
	return bits.Test(gb.Memory.Read(TMC), 2)
}

func (gb *Gameboy) GetClockFreq() byte {
	return gb.Memory.Read(TMC) & 0x3
}

func (gb *Gameboy) getClockFreqCount() int {
	switch gb.GetClockFreq() {
	case 0:
		return 1024
	case 1:
		return 16
	case 2:
		return 64
	default:
		return 256
	}
}

func (gb *Gameboy) SetClockFreq() {
	gb.TimerCounter = gb.getClockFreqCount()
}

func (gb *Gameboy) dividerRegister(cycles int) {
	gb.CPU.Divider += cycles
	if gb.CPU.Divider >= 255 {
		gb.CPU.Divider -= 255
		gb.Memory.Data[0xFF04]++
	}
}

func (gb *Gameboy) RequestInterrupt(interrupt byte) {
	req := gb.Memory.Read(0xFF0F)
	req = bits.Set(req, interrupt)
	gb.Memory.Write(0xFF0F, req)
}

func (gb *Gameboy) doInterrupts() {
	if gb.InterruptsEnabling {
		gb.InterruptsOn = true
		gb.InterruptsEnabling = false
		return
	}
	if !gb.InterruptsOn && !gb.Halted {
		return
	}

	req := gb.Memory.Read(0xFF0F)
	enabled := gb.Memory.Read(0xFFFF)

	if req > 0 {
		var i byte
		for i = 0; i < 5; i++ {
			if bits.Test(req, i) && bits.Test(enabled, i) {
				gb.serviceInterrupt(i)
			}
		}
	}
}

// Address that should be jumped to by interrupt.
var interrupt_addresses = map[byte]uint16{
	0: 0x40, // V-Blank
	1: 0x48, // LCDC Status
	2: 0x50, // Timer Overflow
	3: 0x58, // Serial Transfer
	4: 0x60, // Hi-Lo P10-P13
}

// Called if an interrupt has been raised. Will check if interrupts are
// enabled and will jump to the interrupt address.
func (gb *Gameboy) serviceInterrupt(interrupt byte) {
	// If was halted without interrupts, do not jump or reset IF
	if !gb.InterruptsOn && gb.Halted {
		gb.Halted = false
		return
	}
	gb.InterruptsOn = false
	gb.Halted = false

	req := gb.Memory.Read(0xFF0F)
	req = bits.Reset(req, interrupt)
	gb.Memory.Write(0xFF0F, req)

	gb.pushStack(gb.CPU.PC)
	gb.CPU.PC = interrupt_addresses[interrupt]
}

// Push a 16 bit value onto the stack and decrement SP.
func (gb *Gameboy) pushStack(address uint16) {
	sp := gb.CPU.SP.HiLo()
	gb.Memory.Write(sp-1, byte(uint16(address&0xFF00)>>8))
	gb.Memory.Write(sp-2, byte(address&0xFF))
	gb.CPU.SP.Set(gb.CPU.SP.HiLo() - 2)
}

// Pop the next 16 bit value off the stack and increment SP.
func (gb *Gameboy) popStack() uint16 {
	sp := gb.CPU.SP.HiLo()
	byte1 := uint16(gb.Memory.Read(sp))
	byte2 := uint16(gb.Memory.Read(sp+1)) << 8
	gb.CPU.SP.Set(gb.CPU.SP.HiLo() + 2)
	return byte1 | byte2
}

func (gb *Gameboy) updateGraphics(cycles int) {
	gb.setLCDStatus()

	if !gb.IsLCDEnabled() {
		return
	}
	gb.ScanlineCounter -= cycles

	if gb.ScanlineCounter <= 0 {
		gb.Memory.Data[0xFF44]++
		current_line := gb.Memory.Read(0xFF44)
		gb.ScanlineCounter += 456 * gb.getSpeed()

		if current_line == 144 {
			gb.RequestInterrupt(0)
		} else if current_line > 153 {
			gb.PreparedData = gb.ScreenData
			gb.ScreenData = [160][144][3]uint8{}
			gb.Memory.Data[0xFF44] = 0
			gb.DrawScanline(0)
		} else if current_line < 144 {
			gb.DrawScanline(current_line)
		}
	}
}

func (gb *Gameboy) setLCDStatus() {
	status := gb.Memory.Read(0xFF41)

	if !gb.IsLCDEnabled() {
		// TODO: Set screen to white in this instance
		gb.ScanlineCounter = 456
		gb.Memory.Data[0xFF44] = 0
		status &= 252
		// TODO: Check this is correct
		// We aren't in a mode so reset the values
		status = bits.Reset(status, 0)
		status = bits.Reset(status, 1)
		gb.Memory.Write(0xFF41, status)
		return
	}

	currentLine := gb.Memory.Read(0xFF44)
	currentMode := status & 0x3

	var mode byte = 0
	requestInterrupt := false

	if currentLine >= 144 {
		mode = 1
		status = bits.Set(status, 0)
		status = bits.Reset(status, 1)
		requestInterrupt = bits.Test(status, 4)
	} else {
		mode2Bounds := 456 - 80
		mode3Bounds := mode2Bounds - 172

		if gb.ScanlineCounter >= mode2Bounds {
			mode = 2
			status = bits.Reset(status, 0)
			status = bits.Set(status, 1)
			requestInterrupt = bits.Test(status, 5)
		} else if gb.ScanlineCounter >= mode3Bounds {
			mode = 3
			status = bits.Set(status, 0)
			status = bits.Set(status, 1)
		} else {
			mode = 0
			status = bits.Reset(status, 0)
			status = bits.Reset(status, 1)
			requestInterrupt = bits.Test(status, 3)
			if mode != currentMode {
				gb.Memory.hbHDMATransfer()
			}
		}
	}

	if requestInterrupt && mode != currentMode {
		gb.RequestInterrupt(1)
	}

	// Check is LYC == LY (coincidence flag)
	if gb.Memory.Read(0xFF44) == gb.Memory.Read(0xFF45) {
		status = bits.Set(status, 2)
		// If enabled request an interrupt for this
		if bits.Test(status, 6) {
			gb.RequestInterrupt(1)
		}
	} else {
		status = bits.Reset(status, 2)
	}

	gb.Memory.Write(0xFF41, status)
}

func (gb *Gameboy) IsLCDEnabled() bool {
	return bits.Test(gb.Memory.Read(0xFF40), 7)
}

func (gb *Gameboy) DrawScanline(scanline byte) {
	control := gb.Memory.Read(0xFF40)
	if bits.Test(control, 0) && !gb.Debug.HideBackground {
		gb.RenderTiles(control, scanline)
	}

	if bits.Test(control, 1) && !gb.Debug.HideSprites {
		gb.RenderSprites(control, int32(scanline))
	}
}

func (gb *Gameboy) RenderTiles(lcdControl byte, scanline byte) {
	unsig := false
	tileData := uint16(0x8800)

	scrollY := gb.Memory.Read(0xFF42)
	scrollX := gb.Memory.Read(0xFF43)
	windowY := gb.Memory.Read(0xFF4A)
	windowX := gb.Memory.Read(0xFF4B) - 7

	usingWindow := false

	if bits.Test(lcdControl, 5) {
		// is current scanline we're drawing within windows Y position?
		if windowY <= gb.Memory.Read(0xFF44) {
			usingWindow = true
		}
	}

	// Test if we're using unsigned bytes
	if bits.Test(lcdControl, 4) {
		tileData = 0x8000
		unsig = true
	}

	var test_bit byte = 3
	if usingWindow {
		test_bit = 6
	}

	backgroundMemory := uint16(0x9800)
	if bits.Test(lcdControl, test_bit) {
		backgroundMemory = 0x9C00
	}

	// yPos is used to calc which of 32 v-lines the current scanline is drawing
	var yPos byte = 0
	if !usingWindow {
		yPos = scrollY + scanline
	} else {
		yPos = scanline - windowY
	}

	// which of the 8 vertical pixels of the current tile is the scanline on?
	var tileRow uint16 = uint16(yPos/8) * 32

	// start drawing the 160 horizontal pixels for this scanline
	gb.TileScanline = [160]uint8{}
	for pixel := byte(0); pixel < 160; pixel++ {
		xPos := pixel + scrollX

		// Translate the current x pos to window space if necessary
		if usingWindow && pixel >= windowX {
			xPos = pixel - windowX
		}

		// Which of the 32 horizontal tiles does this x_pox fall within?
		tileCol := uint16(xPos / 8)

		// Get the tile identity number
		tileAddress := backgroundMemory + tileRow + tileCol

		// Deduce where this tile id is in memory
		tileLocation := tileData
		if unsig {
			tileNum := int16(gb.Memory.VRAM[tileAddress - 0x8000])
			tileLocation = tileLocation + uint16(tileNum*16)
		} else {
			tileNum := int16(int8(gb.Memory.VRAM[tileAddress - 0x8000]))
			tileLocation = uint16(int32(tileLocation) + int32((tileNum+128)*16))
		}

		bankOffset := uint16(0x8000)

		// Attributes used in CGB mode TODO: check in CGB mode
		/*
		Bit 0-2  Background Palette number  (BGP0-7)
		Bit 5    Horizontal Flip            (0=Normal, 1=Mirror horizontally)
		Bit 6    Vertical Flip              (0=Normal, 1=Mirror vertically)
		Bit 7    BG-to-OAM Priority         (0=Use OAM priority bit, 1=BG Priority
		 */
		tileAttr := gb.Memory.VRAM[tileAddress - 0x6000]
		if gb.IsCGB() && bits.Test(tileAttr, 3) {
			bankOffset = 0x6000
		}

		// Get the tile data from in memory
		var line byte = (yPos % 8) * 2
		if gb.IsCGB() && bits.Test(tileAttr, 6) {
			line = 16 - line
		}
		data1 := gb.Memory.VRAM[tileLocation+uint16(line) - bankOffset]
		data2 := gb.Memory.VRAM[tileLocation+uint16(line)+1 - bankOffset]

		// TODO: Fix this
		if gb.IsCGB() && bits.Test(tileAttr, 5) {
			xPos = 7 - xPos
		}
		colourBit := byte(int8((xPos%8)-7) * -1)
		colourNum := (bits.Val(data2, colourBit) << 1) | bits.Val(data1, colourBit)

		// TODO:

		// Set the pixel
		if gb.IsCGB() {
			cgbPalette := tileAttr & 0x7
			red, green, blue := gb.BGPalette.get(cgbPalette, colourNum)
			gb.SetPixel(pixel, scanline, red, green, blue, true)
		} else {
			red, green, blue := gb.GetColour(colourNum, 0xFF47)
			gb.SetPixel(pixel, scanline, red, green, blue, true)
		}

		// Store for the current scanline so sprite priority can be managed
		gb.TileScanline[pixel] = colourNum
	}
}

func (gb *Gameboy) GetColour(colourNum byte, address uint16) (uint8, uint8, uint8) {
	var hi, lo byte = 0, 0
	switch colourNum {
	case 0:
		hi, lo = 1, 0
	case 1:
		hi, lo = 3, 2
	case 2:
		hi, lo = 5, 4
	case 3:
		hi, lo = 7, 6
	}

	palette := gb.Memory.Read(address)
	col := (bits.Val(palette, hi) << 1) | bits.Val(palette, lo)

	return GetPaletteColour(col)
}

// Render the sprites to the screen. Takes the lcdControl register
// and current scanline to write to the correct place
func (gb *Gameboy) RenderSprites(lcdControl byte, scanline int32) {
	var ySize int32 = 8
	if bits.Test(lcdControl, 2) {
		ySize = 16
	}

	for sprite := 0; sprite < 40; sprite++ {
		// Load sprite data from memory. Note: for speed purposes
		// we are accessing the Data array directly instead of using
		// the read() method.
		index := sprite * 4
		yPos := int32(gb.Memory.Read(uint16(0xFE00+index))) - 16
		xPos := gb.Memory.Read(uint16(0xFE00+index+1)) - 8
		tileLocation := gb.Memory.Read(uint16(0xFE00+index+2))
		attributes := gb.Memory.Read(uint16(0xFE00+index+3))

		yFlip := bits.Test(attributes, 6)
		xFlip := bits.Test(attributes, 5)
		priority := !bits.Test(attributes, 7)

		// Bank the sprite data in is (CGB only)
		var bank uint16 = 0
		if gb.IsCGB() {
			bank = uint16((attributes & 0x8) >> 3)
		}

		if scanline >= yPos && scanline < (yPos+ySize) {
			// Set the line to draw based on if the sprite is flipped on the y
			line := scanline - yPos
			if yFlip {
				line = (line - ySize) * -1
			}

			// Load the data containing the sprite data for this line
			dataAddress := (uint16(tileLocation) * 16) + uint16(line*2) + (bank * 0x2000)
			data1 := gb.Memory.VRAM[dataAddress]
			data2 := gb.Memory.VRAM[dataAddress+1]

			// Draw the line of the sprite
			for tilePixel := byte(0); tilePixel < 8; tilePixel++ {
				colourBit := tilePixel
				if xFlip {
					colourBit = byte(int8(colourBit-7) * -1)
				}

				// Find the colour value by combining the data bits
				colourNum := (bits.Val(data2, colourBit) << 1) | bits.Val(data1, colourBit)

				// Colour 0 is transparent for sprites
				if colourNum == 0 {
					continue
				}
				pixel := xPos + (7 - tilePixel)

				// Set the pixel if it is in bounds
				if pixel >= 0 && pixel < 160 {
					if gb.IsCGB() {
						cgbPalette := attributes & 0x7
						red, green, blue := gb.SpritePalette.get(cgbPalette, colourNum)
						gb.SetPixel(pixel, byte(scanline), red, green, blue, priority)
					} else {
						// Determine the colour palette to use
						var colourAddress uint16 = 0xFF48
						if bits.Test(attributes, 4) {
							colourAddress = 0xFF49
						}
						red, green, blue := gb.GetColour(colourNum, colourAddress)
						gb.SetPixel(pixel, byte(scanline), red, green, blue, priority)
					}
				}
			}
		}
	}
}

func (gb *Gameboy) SetPixel(x byte, y byte, r uint8, g uint8, b uint8, priority bool) {
	// If priority is false then sprite pixel is only set if tile colour is 0
	if priority || gb.TileScanline[x] == 0 {
		gb.ScreenData[x][y][0] = r
		gb.ScreenData[x][y][1] = g
		gb.ScreenData[x][y][2] = b
	}
}

func (gb *Gameboy) JoypadValue(current byte) byte {
	var in byte = 0xF
	if bits.Test(current, 4) {
		in = gb.InputMask & 0xF
	} else if bits.Test(current, 5) {
		in = (gb.InputMask >> 4) & 0xF
	}
	return current | 0xc0 | in
}

func (gb *Gameboy) IsGameLoaded() bool {
	return gb.Memory != nil && gb.Memory.Cart != nil
}

// Returns if we are using CGB features
func (gb *Gameboy) IsCGB() bool {
	return gb.CGBMode
}

func (gb *Gameboy) Init(romFile string, enableCGB bool) error {
	gb.ExecutionPaused = false

	// Initialise the CPU
	gb.CPU = &CPU{}
	gb.CPU.Init(enableCGB)

	// Initialise the memory
	gb.Memory = &Memory{}
	gb.Memory.Init(gb)

	gb.Sound = &Sound{}
	gb.Sound.Init(gb)

	// Load the ROM file
	hasCGB, err := gb.Memory.LoadCart(romFile, enableCGB)
	if err != nil {
		return fmt.Errorf("could not open rom file: %s", err)
	}

	gb.CGBMode = enableCGB && hasCGB

	gb.Debug = DebugFlags{}
	gb.ScanlineCounter = 456
	gb.TimerCounter = 1024
	gb.InputMask = 0xFF

	gb.CBInst = gb.CBInstructions()

	gb.SpritePalette = NewPalette()
	gb.BGPalette = NewPalette()

	return nil
}
