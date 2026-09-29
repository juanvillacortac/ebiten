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

// Package sdl2 binds the part of SDL2 that a game needs for a window, an OpenGL
// ES context, the window's events, and game controllers. The library is loaded
// at run time, so a system without SDL2 still runs the other backends.
//
// SDL2 picks its video driver itself: a window on X11 or Wayland, or the whole
// display through KMS/DRM where there is no display server, as on handheld
// consoles.
package sdl2

import (
	"errors"
	"fmt"

	"github.com/ebitengine/purego"
)

const (
	InitVideo  = 0x00000020
	InitEvents = 0x00004000
)

const (
	WindowFullscreen        = 0x00000001
	WindowOpenGL            = 0x00000002
	WindowShown             = 0x00000004
	WindowResizable         = 0x00000020
	WindowFullscreenDesktop = WindowFullscreen | 0x00001000
	WindowAllowHighDPI      = 0x00002000

	WindowPosCentered = 0x2fff0000
)

const (
	GLRedSize             = 0
	GLGreenSize           = 1
	GLBlueSize            = 2
	GLAlphaSize           = 3
	GLDoubleBuffer        = 5
	GLDepthSize           = 6
	GLStencilSize         = 7
	GLContextMajorVersion = 17
	GLContextMinorVersion = 18
	GLContextProfileMask  = 21

	GLContextProfileES = 0x0004
)

// Event types.
const (
	EventQuit            = 0x100
	EventWindow          = 0x200
	EventKeyDown         = 0x300
	EventKeyUp           = 0x301
	EventTextInput       = 0x303
	EventMouseMotion     = 0x400
	EventMouseButtonDown = 0x401
	EventMouseButtonUp   = 0x402
	EventMouseWheel      = 0x403
)

// Window event IDs.
const (
	WindowEventResized     = 5
	WindowEventSizeChanged = 6
	WindowEventFocusGained = 12
	WindowEventFocusLost   = 13
	WindowEventClose       = 14
)

// Event is an SDL_Event. The union is read field by field at the offsets SDL2
// fixes for every platform.
type Event [56]byte

func (e *Event) u8(offset int) uint8 {
	return e[offset]
}

func (e *Event) u16(offset int) uint16 {
	return uint16(e[offset]) | uint16(e[offset+1])<<8
}

func (e *Event) u32(offset int) uint32 {
	return uint32(e[offset]) | uint32(e[offset+1])<<8 | uint32(e[offset+2])<<16 | uint32(e[offset+3])<<24
}

func (e *Event) i32(offset int) int32 {
	return int32(e.u32(offset))
}

// Type returns the event type.
func (e *Event) Type() uint32 {
	return e.u32(0)
}

// WindowEvent returns the window event ID and its two data fields.
func (e *Event) WindowEvent() (event uint8, data1, data2 int32) {
	return e.u8(12), e.i32(16), e.i32(20)
}

// Key returns the scancode of a keyboard event and whether it is a repeat.
func (e *Event) Key() (scancode int32, repeat bool) {
	return e.i32(16), e.u8(13) != 0
}

// Text returns the UTF-8 text of a text input event.
func (e *Event) Text() string {
	b := e[12:44]
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// MouseMotion returns the cursor position of a mouse motion event.
func (e *Event) MouseMotion() (x, y int32) {
	return e.i32(20), e.i32(24)
}

// MouseButton returns the button and the cursor position of a mouse button event.
func (e *Event) MouseButton() (button uint8, x, y int32) {
	return e.u8(16), e.i32(20), e.i32(24)
}

// MouseWheel returns the scroll amounts of a mouse wheel event, with the
// direction already applied.
func (e *Event) MouseWheel() (x, y int32) {
	x, y = e.i32(16), e.i32(20)
	// SDL_MOUSEWHEEL_FLIPPED
	if e.u32(24) == 1 {
		x, y = -x, -y
	}
	return x, y
}

// DisplayMode is an SDL_DisplayMode.
type DisplayMode struct {
	Format      uint32
	W           int32
	H           int32
	RefreshRate int32
	DriverData  uintptr
}

var (
	lib uintptr

	sdlSetHint               func(name, value string) int32
	sdlInit                  func(flags uint32) int32
	sdlQuit                  func()
	sdlGetError              func() string
	sdlGetCurrentVideoDriver func() string
	sdlGetCurrentDisplayMode func(displayIndex int32, mode *DisplayMode) int32
	sdlCreateWindow          func(title string, x, y, w, h int32, flags uint32) uintptr
	sdlDestroyWindow         func(window uintptr)
	sdlSetWindowTitle        func(window uintptr, title string)
	sdlSetWindowFullscreen   func(window uintptr, flags uint32) int32
	sdlGetWindowFlags        func(window uintptr) uint32
	sdlGetWindowSize         func(window uintptr, w, h *int32)
	sdlShowCursor            func(toggle int32) int32
	sdlGLSetAttribute        func(attr, value int32) int32
	sdlGLCreateContext       func(window uintptr) uintptr
	sdlGLDeleteContext       func(context uintptr)
	sdlGLMakeCurrent         func(window, context uintptr) int32
	sdlGLSetSwapInterval     func(interval int32) int32
	sdlGLSwapWindow          func(window uintptr)
	sdlGLGetDrawableSize     func(window uintptr, w, h *int32)
	sdlPollEvent             func(event *Event) int32
)

// Load loads SDL2. It is safe to call Load more than once.
func Load() error {
	if lib != 0 {
		return nil
	}

	var errs []error
	for _, name := range []string{"libSDL2-2.0.so.0", "libSDL2-2.0.so", "libSDL2.so"} {
		l, err := purego.Dlopen(name, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lib = l
		break
	}
	if lib == 0 {
		return fmt.Errorf("sdl2: failed to load libSDL2: %w", errors.Join(errs...))
	}

	for _, f := range []struct {
		ptr  any
		name string
	}{
		{&sdlSetHint, "SDL_SetHint"},
		{&sdlInit, "SDL_Init"},
		{&sdlQuit, "SDL_Quit"},
		{&sdlGetError, "SDL_GetError"},
		{&sdlGetCurrentVideoDriver, "SDL_GetCurrentVideoDriver"},
		{&sdlGetCurrentDisplayMode, "SDL_GetCurrentDisplayMode"},
		{&sdlCreateWindow, "SDL_CreateWindow"},
		{&sdlDestroyWindow, "SDL_DestroyWindow"},
		{&sdlSetWindowTitle, "SDL_SetWindowTitle"},
		{&sdlSetWindowFullscreen, "SDL_SetWindowFullscreen"},
		{&sdlGetWindowFlags, "SDL_GetWindowFlags"},
		{&sdlGetWindowSize, "SDL_GetWindowSize"},
		{&sdlShowCursor, "SDL_ShowCursor"},
		{&sdlGLSetAttribute, "SDL_GL_SetAttribute"},
		{&sdlGLCreateContext, "SDL_GL_CreateContext"},
		{&sdlGLDeleteContext, "SDL_GL_DeleteContext"},
		{&sdlGLMakeCurrent, "SDL_GL_MakeCurrent"},
		{&sdlGLSetSwapInterval, "SDL_GL_SetSwapInterval"},
		{&sdlGLSwapWindow, "SDL_GL_SwapWindow"},
		{&sdlGLGetDrawableSize, "SDL_GL_GetDrawableSize"},
		{&sdlPollEvent, "SDL_PollEvent"},
		{&sdlFree, "SDL_free"},
		{&sdlIsGameController, "SDL_IsGameController"},
		{&sdlGameControllerOpen, "SDL_GameControllerOpen"},
		{&sdlGameControllerClose, "SDL_GameControllerClose"},
		{&sdlGameControllerName, "SDL_GameControllerName"},
		{&sdlGameControllerMapping, "SDL_GameControllerMapping"},
		{&sdlGameControllerGetJoy, "SDL_GameControllerGetJoystick"},
		{&sdlJoystickInstanceID, "SDL_JoystickInstanceID"},
		{&sdlGameControllerHasButton, "SDL_GameControllerHasButton"},
		{&sdlGameControllerHasAxis, "SDL_GameControllerHasAxis"},
		{&sdlGameControllerGetButton, "SDL_GameControllerGetButton"},
		{&sdlGameControllerGetAxis, "SDL_GameControllerGetAxis"},
		{&sdlGameControllerRumble, "SDL_GameControllerRumble"},
		{&sdlGameControllerGetVendor, "SDL_GameControllerGetVendor"},
		{&sdlGameControllerGetProd, "SDL_GameControllerGetProduct"},
		{&sdlGameControllerGetVer, "SDL_GameControllerGetProductVersion"},
	} {
		sym, err := purego.Dlsym(lib, f.name)
		if err != nil || sym == 0 {
			_ = purego.Dlclose(lib)
			lib = 0
			return fmt.Errorf("sdl2: %s not found in libSDL2 (SDL 2.0.18 or later is required): %w", f.name, err)
		}
		purego.RegisterFunc(f.ptr, sym)
	}
	return nil
}

// sdlError is an error reported by SDL.
type sdlError struct {
	fn  string
	msg string
}

func (e *sdlError) Error() string {
	return fmt.Sprintf("sdl2: %s failed: %s", e.fn, e.msg)
}

func lastError(fn string) error {
	return &sdlError{fn: fn, msg: sdlGetError()}
}

func SetHint(name, value string) bool {
	return sdlSetHint(name, value) != 0
}

func Init(flags uint32) error {
	if sdlInit(flags) != 0 {
		return lastError("SDL_Init")
	}
	return nil
}

func Quit() {
	sdlQuit()
}

// CurrentVideoDriver returns the name of the video driver in use, such as
// "x11", "wayland", or "KMSDRM".
func CurrentVideoDriver() string {
	return sdlGetCurrentVideoDriver()
}

func CurrentDisplayMode(displayIndex int) (DisplayMode, error) {
	var m DisplayMode
	if sdlGetCurrentDisplayMode(int32(displayIndex), &m) != 0 {
		return DisplayMode{}, lastError("SDL_GetCurrentDisplayMode")
	}
	return m, nil
}

func GLSetAttribute(attr, value int) error {
	if sdlGLSetAttribute(int32(attr), int32(value)) != 0 {
		return lastError("SDL_GL_SetAttribute")
	}
	return nil
}

func ShowCursor(show bool) {
	var toggle int32
	if show {
		toggle = 1
	}
	sdlShowCursor(toggle)
}

func PollEvent(event *Event) bool {
	return sdlPollEvent(event) != 0
}

// Window is an SDL_Window.
type Window uintptr

func CreateWindow(title string, x, y, w, h int, flags uint32) (Window, error) {
	win := sdlCreateWindow(title, int32(x), int32(y), int32(w), int32(h), flags)
	if win == 0 {
		return 0, lastError("SDL_CreateWindow")
	}
	return Window(win), nil
}

func (w Window) Destroy() {
	sdlDestroyWindow(uintptr(w))
}

func (w Window) SetTitle(title string) {
	sdlSetWindowTitle(uintptr(w), title)
}

func (w Window) SetFullscreen(flags uint32) error {
	if sdlSetWindowFullscreen(uintptr(w), flags) != 0 {
		return lastError("SDL_SetWindowFullscreen")
	}
	return nil
}

func (w Window) Flags() uint32 {
	return sdlGetWindowFlags(uintptr(w))
}

// Size returns the size of the window in screen coordinates.
func (w Window) Size() (width, height int) {
	var cw, ch int32
	sdlGetWindowSize(uintptr(w), &cw, &ch)
	return int(cw), int(ch)
}

// DrawableSize returns the size of the window's drawable in pixels.
func (w Window) DrawableSize() (width, height int) {
	var cw, ch int32
	sdlGLGetDrawableSize(uintptr(w), &cw, &ch)
	return int(cw), int(ch)
}

// GLContext is an SDL_GLContext.
type GLContext uintptr

func GLCreateContext(w Window) (GLContext, error) {
	c := sdlGLCreateContext(uintptr(w))
	if c == 0 {
		return 0, lastError("SDL_GL_CreateContext")
	}
	return GLContext(c), nil
}

func GLDeleteContext(c GLContext) {
	sdlGLDeleteContext(uintptr(c))
}

// GLMakeCurrent binds c to the calling thread with w as its drawable. A zero c
// releases the context current on the calling thread.
func GLMakeCurrent(w Window, c GLContext) error {
	if sdlGLMakeCurrent(uintptr(w), uintptr(c)) != 0 {
		return lastError("SDL_GL_MakeCurrent")
	}
	return nil
}

func GLSetSwapInterval(interval int) error {
	if sdlGLSetSwapInterval(int32(interval)) != 0 {
		return lastError("SDL_GL_SetSwapInterval")
	}
	return nil
}

func GLSwapWindow(w Window) {
	sdlGLSwapWindow(uintptr(w))
}
