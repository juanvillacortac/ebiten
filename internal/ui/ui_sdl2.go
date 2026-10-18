// Copyright 2026 The Ebitengine Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux && !android && !nintendosdk && !playstation5

package ui

import (
	stdcontext "context"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/hajimehoshi/ebiten/v2/internal/gamepad"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicscommand"
	"github.com/hajimehoshi/ebiten/v2/internal/graphicsdriver/opengl"
	"github.com/hajimehoshi/ebiten/v2/internal/sdl2"
	"github.com/hajimehoshi/ebiten/v2/internal/thread"
)

var (
	_ uiBackend            = (*sdl2Backend)(nil)
	_ virtualMonitorSource = (*sdl2Backend)(nil)
	_ opengl.Presenter     = (*sdl2Presenter)(nil)
)

// sdl2Backend runs the game on SDL2. SDL2 chooses the video driver, so the same
// backend opens a window under a display server and takes the whole display
// through KMS/DRM on a system that has none, like a handheld console.
//
// Gamepads are read through SDL too (see ui_sdl2_gamepad.go), so the mappings
// SDL is given decide their standard layout.
type sdl2Backend struct {
	*UserInterface

	window    sdl2.Window
	presenter *sdl2Presenter

	// kmsdrm reports that SDL presents through KMS/DRM, where the window is the
	// display and nothing can resize it.
	kmsdrm bool

	// displayWidth and displayHeight are the size of the display the window is
	// on, in pixels.
	displayWidth  int
	displayHeight int

	// The fields below are written on the main thread while polling events, and
	// read on the game thread after the poll returns.
	windowWidth    int
	windowHeight   int
	drawableWidth  int
	drawableHeight int
	focused        bool
	closeRequested bool

	gamepads sdl2Gamepads

	frameCh chan struct{}

	// idlePoll is how long FPSModeVsyncOffMinimum waits between event polls.
	// It doubles while nothing happens, up to maxIdlePoll, and goes back to a
	// frame's worth when a frame runs.
	idlePoll time.Duration

	monitor *Monitor

	// inputState and the raw cursor position are guarded by mu.
	inputState InputState
	rawCursorX float64
	rawCursorY float64
	mu         sync.Mutex
}

// sdl2BackendRequested reports whether the SDL2 backend is asked for explicitly,
// ahead of a window system that GLFW could use.
func sdl2BackendRequested() bool {
	return strings.EqualFold(os.Getenv("EBITENGINE_UI_BACKEND"), "sdl2")
}

// maybeNewSDL2Backend returns a backend running on SDL2, or the reason SDL2
// cannot be used.
func maybeNewSDL2Backend(u *UserInterface) (*sdl2Backend, error) {
	if err := sdl2.Load(); err != nil {
		return nil, err
	}
	b := &sdl2Backend{
		UserInterface: u,
		focused:       true,
		gamepads:      newSDL2Gamepads(),
		frameCh:       make(chan struct{}, 1),
		idlePoll:      minIdlePoll,
	}
	b.monitor = &Monitor{virtual: b}
	return b, nil
}

func (b *sdl2Backend) run(game Game, options *RunOptions) error {
	if options == nil {
		options = &RunOptions{}
	}

	b.mainThread = thread.NewOSThread()
	graphicscommand.SetOSThreadAsRenderThread()

	b.context = newContext(game, options.ScreenTransparent)

	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	defer cancel()

	var wg errgroup.Group

	// Run the render thread.
	wg.Go(func() error {
		defer cancel()

		graphicscommand.LoopRenderThread(ctx)
		return nil
	})

	// Run the game thread.
	wg.Go(func() error {
		defer cancel()

		type args struct {
			b       *sdl2Backend
			options *RunOptions
		}
		if err := thread.CallWithArgAndResult(b.mainThread, func(a args) error {
			return a.b.initOnMainThread(a.options)
		}, args{b: b, options: options}); err != nil {
			return err
		}

		defer b.setRunningBackend(nil)

		return b.loopGame()
	})

	// Run the main thread. SDL's video and events are used only from here.
	_ = b.mainThread.LoopAndStop(ctx)
	return wg.Wait()
}

func (b *sdl2Backend) initOnMainThread(options *RunOptions) (err error) {
	// Present through EGL everywhere, so that the context SDL creates is the one
	// the OpenGL ES functions loaded from libGLESv2 act on.
	sdl2.SetHint("SDL_OPENGL_ES_DRIVER", "1")
	sdl2.SetHint("SDL_VIDEO_X11_FORCE_EGL", "1")
	// A handheld has no window focus to lose, and a controller must keep working
	// if a display server says otherwise.
	sdl2.SetHint("SDL_JOYSTICK_ALLOW_BACKGROUND_EVENTS", "1")
	// With the kernel's hid_nintendo driver loaded, SDL's own HIDAPI driver for
	// Switch controllers fights it: each open waits about a second for a
	// handshake that never comes, the controller is dropped and added again,
	// and the main thread stalls on every open, down to a few frames a second.
	// The kernel driver works through evdev. An environment variable of the
	// same name still overrides this.
	sdl2.SetHint("SDL_JOYSTICK_HIDAPI_SWITCH", "0")

	if err := sdl2.Init(sdl2.InitVideo | sdl2.InitGameController | sdl2.InitEvents); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			b.closeOnMainThread()
		}
	}()

	b.kmsdrm = strings.EqualFold(sdl2.CurrentVideoDriver(), "kmsdrm")

	mode, err := sdl2.CurrentDisplayMode(0)
	if err != nil {
		return err
	}
	b.displayWidth, b.displayHeight = int(mode.W), int(mode.H)

	for _, a := range []struct{ attr, value int }{
		{sdl2.GLContextProfileMask, sdl2.GLContextProfileES},
		// The graphics driver requires OpenGL ES 3 for its GLSL ES 300 shaders.
		{sdl2.GLContextMajorVersion, 3},
		{sdl2.GLContextMinorVersion, 0},
		{sdl2.GLDoubleBuffer, 1},
		// These are minimums: a 16-bit display of a handheld matches, and a
		// 32-bit one still gets 8 bits per channel.
		{sdl2.GLRedSize, 5},
		{sdl2.GLGreenSize, 6},
		{sdl2.GLBlueSize, 5},
		{sdl2.GLAlphaSize, 0},
		// The graphics driver draws to its own framebuffers; the default one
		// only receives the final screen.
		{sdl2.GLDepthSize, 0},
		{sdl2.GLStencilSize, 0},
	} {
		if err := sdl2.GLSetAttribute(a.attr, a.value); err != nil {
			return err
		}
	}

	title := b.desktopWindow.title.Load()
	flags := uint32(sdl2.WindowOpenGL | sdl2.WindowShown)
	var w, h int
	if b.kmsdrm {
		// The window is the display.
		w, h = b.displayWidth, b.displayHeight
		flags |= sdl2.WindowFullscreen
	} else {
		w, h = b.desktopWindow.getWindowSizeInDIP()
		flags |= sdl2.WindowResizable | sdl2.WindowAllowHighDPI
		if b.isInitFullscreen() {
			flags |= sdl2.WindowFullscreenDesktop
		}
	}
	win, err := sdl2.CreateWindow(title, sdl2.WindowPosCentered, sdl2.WindowPosCentered, w, h, flags)
	if err != nil {
		return err
	}
	b.window = win
	b.desktopWindow.title.markApplied(&title)

	c, err := sdl2.GLCreateContext(win)
	if err != nil {
		return err
	}
	b.presenter = &sdl2Presenter{
		window:       win,
		context:      c,
		swapInterval: -1,
	}
	// SDL makes the new context current here, but the graphics driver makes it
	// current on the render thread. A context can be current on one thread only.
	if err := sdl2.GLMakeCurrent(win, 0); err != nil {
		return err
	}

	g, lib, err := newGraphicsDriver(&graphicsDriverCreatorImpl{}, options.GraphicsLibrary)
	if err != nil {
		return err
	}
	p, ok := g.(interface{ SetPresenter(opengl.Presenter) })
	if !ok {
		return fmt.Errorf("ui: the graphics driver cannot present through SDL2")
	}
	p.SetPresenter(b.presenter)

	b.graphicsDriver = g
	b.setGraphicsLibrary(lib)
	graphicscommand.SetVsyncEnabled(FPSModeType(b.fpsMode.Load()) == FPSModeVsyncOn)

	b.applyCursorModeOnMainThread(b.getInitCursorMode())
	b.updateSizesOnMainThread()

	b.setRunningBackend(b)
	b.ScheduleFrame()

	return nil
}

func (b *sdl2Backend) closeOnMainThread() {
	b.gamepads.closeAll()
	if b.presenter != nil {
		sdl2.GLDeleteContext(b.presenter.context)
		b.presenter = nil
	}
	if b.window != 0 {
		b.window.Destroy()
		b.window = 0
	}
	sdl2.Quit()
}

func (b *sdl2Backend) loopGame() error {
	defer func() {
		graphicscommand.Terminate()
		thread.CallWithArg(b.mainThread, func(b *sdl2Backend) {
			b.closeOnMainThread()
			b.setTerminated()
		}, b)
	}()

	for {
		if err := b.updateGame(); err != nil {
			return err
		}
	}
}

// slowFrame is the frame time above which updateGame logs where the time
// went (polling, the frame itself and, inside it, SDL_GL_SwapWindow), at most
// once a second: on KMS/DRM handhelds, the hitches that the game itself
// cannot see (its Update and Draw are short) are here.
const slowFrame = 40 * time.Millisecond

// The bounds of sdl2Backend.idlePoll. SDL_WaitEvent cannot replace polling
// here: with a game controller open, or on video drivers without a native wait
// such as KMSDRM, SDL2 implements it as a loop that polls every millisecond.
// Instead, FPSModeVsyncOffMinimum blocks on ScheduleFrame and polls for events
// less and less often while the application is idle: a static application
// wakes up 4 times a second instead of 60, at the cost of up to 250ms of
// latency for the first event after a long idle.
const (
	minIdlePoll = time.Second / 60
	maxIdlePoll = time.Second / 4
)

// lastSwap is how long the last SDL_GL_SwapWindow took, in nanoseconds.
var lastSwap atomic.Int64

// lastSlowLog is when updateGame last logged a slow frame.
var lastSlowLog time.Time

func (b *sdl2Backend) updateGame() error {
	start := time.Now()
	var polled, padded time.Time
	defer func() {
		total := time.Since(start)
		if total < slowFrame || time.Since(lastSlowLog) < time.Second {
			return
		}
		lastSlowLog = time.Now()
		ms := func(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond)) }
		log.Printf("sdl2: slow frame %s: poll %s, gamepads %s, frame %s (SDL_GL_SwapWindow %s)",
			ms(total), ms(polled.Sub(start)), ms(padded.Sub(polled)), ms(time.Since(padded)), ms(time.Duration(lastSwap.Load())))
	}()
	for {
		thread.CallWithArg(b.mainThread, (*sdl2Backend).pollOnMainThread, b)
		if b.closeRequested {
			return RegularTermination
		}
		// In this mode a frame runs only when something asks for one. An event
		// asks for one too, so keep polling while waiting, less often the
		// longer nothing happens.
		if FPSModeType(b.fpsMode.Load()) != FPSModeVsyncOffMinimum {
			break
		}
		timer := time.NewTimer(b.idlePoll)
		select {
		case <-b.frameCh:
			timer.Stop()
		case <-timer.C:
			b.idlePoll = min(2*b.idlePoll, maxIdlePoll)
			continue
		}
		break
	}
	b.idlePoll = minIdlePoll

	polled = time.Now()
	if err := gamepad.Update(0, b.gamepads.states); err != nil {
		return err
	}
	padded = time.Now()

	s := b.deviceScaleFactor()
	return b.context.updateFrame(b.graphicsDriver, float64(b.windowWidth), float64(b.windowHeight), b.drawableWidth, b.drawableHeight, s, b.UserInterface, true)
}

// pollOnMainThread applies the pending window settings, handles the pending
// events, and snapshots the gamepads.
func (b *sdl2Backend) pollOnMainThread() {
	_ = b.desktopWindow.title.apply(func(title string) error {
		b.window.SetTitle(title)
		return nil
	})

	b.gamepads.rumble()

	var ev sdl2.Event
	var gotEvent bool
	for sdl2.PollEvent(&ev) {
		gotEvent = true
		b.handleEventOnMainThread(&ev)
	}
	if gotEvent {
		b.ScheduleFrame()
	}

	b.updateSizesOnMainThread()
	if b.gamepads.snapshot() {
		b.ScheduleFrame()
	}
}

func (b *sdl2Backend) updateSizesOnMainThread() {
	b.windowWidth, b.windowHeight = b.window.Size()
	b.drawableWidth, b.drawableHeight = b.window.DrawableSize()
}

func (b *sdl2Backend) handleEventOnMainThread(ev *sdl2.Event) {
	if b.gamepads.handleEvent(ev) {
		return
	}
	switch ev.Type() {
	case sdl2.EventQuit:
		b.requestClose()

	case sdl2.EventWindow:
		e, _, _ := ev.WindowEvent()
		switch e {
		case sdl2.WindowEventClose:
			b.requestClose()
		case sdl2.WindowEventFocusGained:
			b.focused = true
		case sdl2.WindowEventFocusLost:
			b.focused = false
			b.mu.Lock()
			b.inputState.releaseAllButtons(b.inputState.nextInputTime())
			b.mu.Unlock()
		}

	case sdl2.EventKeyDown, sdl2.EventKeyUp:
		scancode, repeat := ev.Key()
		if repeat {
			return
		}
		key, ok := sdl2ScancodeToKey[scancode]
		if !ok {
			return
		}
		b.mu.Lock()
		if ev.Type() == sdl2.EventKeyDown {
			b.inputState.setKeyPressed(key, b.inputState.nextInputTime())
		} else {
			b.inputState.setKeyReleased(key, b.inputState.nextInputTime())
		}
		b.mu.Unlock()

	case sdl2.EventTextInput:
		b.mu.Lock()
		for _, r := range ev.Text() {
			b.inputState.appendRune(r)
		}
		b.mu.Unlock()

	case sdl2.EventMouseMotion:
		x, y := ev.MouseMotion()
		b.mu.Lock()
		b.rawCursorX, b.rawCursorY = float64(x), float64(y)
		b.mu.Unlock()

	case sdl2.EventMouseButtonDown, sdl2.EventMouseButtonUp:
		button, x, y := ev.MouseButton()
		// SDL numbers the buttons from 1: left, middle, right, X1, X2.
		if button < 1 || int(button) > int(MouseButtonMax)+1 {
			return
		}
		mb := MouseButton(button - 1)
		b.mu.Lock()
		b.rawCursorX, b.rawCursorY = float64(x), float64(y)
		if ev.Type() == sdl2.EventMouseButtonDown {
			b.inputState.setMouseButtonPressed(mb, b.inputState.nextInputTime())
		} else {
			b.inputState.setMouseButtonReleased(mb, b.inputState.nextInputTime())
		}
		b.mu.Unlock()

	case sdl2.EventMouseWheel:
		x, y := ev.MouseWheel()
		b.mu.Lock()
		b.inputState.WheelX += float64(x)
		b.inputState.WheelY += float64(y)
		b.inputState.ScrollDeltaX += float64(x) * pixelsPerScrollNotch
		b.inputState.ScrollDeltaY += float64(y) * pixelsPerScrollNotch
		b.mu.Unlock()

	}
}

func (b *sdl2Backend) requestClose() {
	b.mu.Lock()
	b.inputState.WindowBeingClosed = true
	b.mu.Unlock()
	if !b.desktopWindow.isWindowClosingHandled() {
		b.closeRequested = true
	}
}

// deviceScaleFactor implements virtualMonitorSource.
func (b *sdl2Backend) deviceScaleFactor() float64 {
	if b.windowWidth <= 0 || b.drawableWidth <= 0 {
		return 1
	}
	return float64(b.drawableWidth) / float64(b.windowWidth)
}

// outsideSize implements virtualMonitorSource. It is the size of the display.
func (b *sdl2Backend) outsideSize() (width, height float64) {
	s := b.deviceScaleFactor()
	return float64(b.displayWidth) / s, float64(b.displayHeight) / s
}

func (b *sdl2Backend) readInputState(inputState *InputState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inputState.copyAndReset(inputState)
}

func (b *sdl2Backend) updateInputStateForFrame(deviceScaleFactor float64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// SDL reports the cursor in screen coordinates, which are the logical
	// pixels of the outside size.
	x, y := b.context.clientPositionToLogicalPosition(b.rawCursorX, b.rawCursorY, deviceScaleFactor)
	if !math.IsNaN(x) && !math.IsNaN(y) {
		b.inputState.CursorX, b.inputState.CursorY = x, y
	}
	return nil
}

func (b *sdl2Backend) KeyName(key Key) string {
	return ""
}

func (b *sdl2Backend) updateIconIfNeeded() error {
	return nil
}

func (b *sdl2Backend) IsFocused() bool {
	return b.focused
}

func (b *sdl2Backend) IsFullscreen() bool {
	if b.kmsdrm {
		return true
	}
	var flags uint32
	b.RunOnMainThread(func() {
		flags = b.window.Flags()
	})
	return flags&sdl2.WindowFullscreen != 0
}

func (b *sdl2Backend) SetFullscreen(fullscreen bool) {
	if b.kmsdrm {
		return
	}
	b.RunOnMainThread(func() {
		var flags uint32
		if fullscreen {
			flags = sdl2.WindowFullscreenDesktop
		}
		_ = b.window.SetFullscreen(flags)
	})
}

func (b *sdl2Backend) CursorMode() CursorMode {
	return CursorMode(b.initCursorMode.Load())
}

func (b *sdl2Backend) SetCursorMode(mode CursorMode) {
	b.setInitCursorMode(mode)
	b.RunOnMainThread(func() {
		b.applyCursorModeOnMainThread(mode)
	})
}

func (b *sdl2Backend) applyCursorModeOnMainThread(mode CursorMode) {
	// A handheld has no pointer to show, even if a mouse is plugged in.
	sdl2.ShowCursor(mode == CursorModeVisible && !b.kmsdrm)
}

func (b *sdl2Backend) applyCursorShape() {
}

func (b *sdl2Backend) applyFPSMode() {
	b.RunOnMainThread(func() {
		graphicscommand.SetVsyncEnabled(FPSModeType(b.fpsMode.Load()) == FPSModeVsyncOn)
	})
	b.ScheduleFrame()
}

func (b *sdl2Backend) ScheduleFrame() {
	select {
	case b.frameCh <- struct{}{}:
	default:
	}
}

func (b *sdl2Backend) Window() backendWindow {
	return &nullWindow{}
}

func (b *sdl2Backend) Monitor() *Monitor {
	return b.monitor
}

func (b *sdl2Backend) appendMonitors(monitors []*Monitor) []*Monitor {
	return append(monitors, b.monitor)
}

func (b *sdl2Backend) RunOnMainThread(f func()) {
	thread.Call(b.mainThread, f)
}

// sdl2Presenter presents through the OpenGL ES context of an SDL window. Its
// methods are called on the render thread.
type sdl2Presenter struct {
	window       sdl2.Window
	context      sdl2.GLContext
	swapInterval int
}

func (p *sdl2Presenter) MakeContextCurrent() error {
	return sdl2.GLMakeCurrent(p.window, p.context)
}

func (p *sdl2Presenter) SwapInterval(interval int) error {
	if p.swapInterval == interval {
		return nil
	}
	p.swapInterval = interval
	// Some KMS/DRM drivers can only flip on vblank. Keep presenting rather
	// than failing: the frame rate is then bound to the display.
	if err := sdl2.GLSetSwapInterval(interval); err != nil && interval != 0 {
		return err
	}
	return nil
}

func (p *sdl2Presenter) SwapBuffers() error {
	start := time.Now()
	sdl2.GLSwapWindow(p.window)
	lastSwap.Store(int64(time.Since(start)))
	return nil
}

// sdl2ScancodeToKey maps SDL scancodes, which are USB HID usages, to keys.
var sdl2ScancodeToKey = func() map[int32]Key {
	m := map[int32]Key{
		40:  KeyEnter,
		41:  KeyEscape,
		42:  KeyBackspace,
		43:  KeyTab,
		44:  KeySpace,
		45:  KeyMinus,
		46:  KeyEqual,
		47:  KeyBracketLeft,
		48:  KeyBracketRight,
		49:  KeyBackslash,
		51:  KeySemicolon,
		52:  KeyQuote,
		53:  KeyBackquote,
		54:  KeyComma,
		55:  KeyPeriod,
		56:  KeySlash,
		57:  KeyCapsLock,
		70:  KeyPrintScreen,
		71:  KeyScrollLock,
		72:  KeyPause,
		73:  KeyInsert,
		74:  KeyHome,
		75:  KeyPageUp,
		76:  KeyDelete,
		77:  KeyEnd,
		78:  KeyPageDown,
		79:  KeyArrowRight,
		80:  KeyArrowLeft,
		81:  KeyArrowDown,
		82:  KeyArrowUp,
		83:  KeyNumLock,
		84:  KeyNumpadDivide,
		85:  KeyNumpadMultiply,
		86:  KeyNumpadSubtract,
		87:  KeyNumpadAdd,
		88:  KeyNumpadEnter,
		98:  KeyNumpad0,
		99:  KeyNumpadDecimal,
		100: KeyIntlBackslash,
		101: KeyContextMenu,
		103: KeyNumpadEqual,
		224: KeyControlLeft,
		225: KeyShiftLeft,
		226: KeyAltLeft,
		227: KeyMetaLeft,
		228: KeyControlRight,
		229: KeyShiftRight,
		230: KeyAltRight,
		231: KeyMetaRight,
	}
	for i := int32(0); i < 26; i++ {
		m[4+i] = KeyA + Key(i)
	}
	for i := int32(0); i < 9; i++ {
		m[30+i] = KeyDigit1 + Key(i)
		m[89+i] = KeyNumpad1 + Key(i)
	}
	m[39] = KeyDigit0
	for i := int32(0); i < 12; i++ {
		m[58+i] = KeyF1 + Key(i)
		m[104+i] = KeyF13 + Key(i)
	}
	return m
}()
