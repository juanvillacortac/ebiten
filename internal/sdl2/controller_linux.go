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

package sdl2

import (
	"fmt"
	"unsafe"
)

// SDL reads the mappings in SDL_GAMECONTROLLERCONFIG by itself, as PortMaster
// sets them per device, and names buttons by position: A is the bottom face
// button whatever its label.

const InitGameController = 0x00002000

const (
	EventControllerAxisMotion     = 0x650
	EventControllerButtonDown     = 0x651
	EventControllerButtonUp       = 0x652
	EventControllerDeviceAdded    = 0x653
	EventControllerDeviceRemoved  = 0x654
	EventControllerDeviceRemapped = 0x655
)

// Game controller buttons, in SDL_GameControllerButton order.
const (
	ControllerButtonA = iota
	ControllerButtonB
	ControllerButtonX
	ControllerButtonY
	ControllerButtonBack
	ControllerButtonGuide
	ControllerButtonStart
	ControllerButtonLeftStick
	ControllerButtonRightStick
	ControllerButtonLeftShoulder
	ControllerButtonRightShoulder
	ControllerButtonDPadUp
	ControllerButtonDPadDown
	ControllerButtonDPadLeft
	ControllerButtonDPadRight
	ControllerButtonCount
)

// Game controller axes, in SDL_GameControllerAxis order.
const (
	ControllerAxisLeftX = iota
	ControllerAxisLeftY
	ControllerAxisRightX
	ControllerAxisRightY
	ControllerAxisTriggerLeft
	ControllerAxisTriggerRight
	ControllerAxisCount
)

var (
	sdlFree                    func(ptr unsafe.Pointer)
	sdlIsGameController        func(joystickIndex int32) int32
	sdlGameControllerOpen      func(joystickIndex int32) uintptr
	sdlGameControllerClose     func(controller uintptr)
	sdlGameControllerName      func(controller uintptr) string
	sdlGameControllerMapping   func(controller uintptr) unsafe.Pointer
	sdlGameControllerGetJoy    func(controller uintptr) uintptr
	sdlJoystickInstanceID      func(joystick uintptr) int32
	sdlGameControllerHasButton func(controller uintptr, button int32) int32
	sdlGameControllerHasAxis   func(controller uintptr, axis int32) int32
	sdlGameControllerGetButton func(controller uintptr, button int32) uint8
	sdlGameControllerGetAxis   func(controller uintptr, axis int32) int16
	sdlGameControllerRumble    func(controller uintptr, low, high uint16, durationMS uint32) int32
	sdlGameControllerGetVendor func(controller uintptr) uint16
	sdlGameControllerGetProd   func(controller uintptr) uint16
	sdlGameControllerGetVer    func(controller uintptr) uint16
)

// ControllerDevice returns the device index (for an added controller) or the
// joystick instance ID (for a removed or remapped one).
func (e *Event) ControllerDevice() int32 {
	return e.i32(8)
}

// ControllerButton is the button of a controller button event.
func (e *Event) ControllerButton() int {
	return int(e.u8(12))
}

// ControllerAxis is the axis of a controller axis event.
func (e *Event) ControllerAxis() int {
	return int(e.u8(12))
}

// ControllerAxisValue is the value of a controller axis event.
func (e *Event) ControllerAxisValue() int16 {
	return int16(e.u16(16))
}

func IsGameController(joystickIndex int) bool {
	return sdlIsGameController(int32(joystickIndex)) != 0
}

// GameController is an SDL_GameController.
type GameController uintptr

func GameControllerOpen(joystickIndex int) (GameController, error) {
	c := sdlGameControllerOpen(int32(joystickIndex))
	if c == 0 {
		return 0, lastError("SDL_GameControllerOpen")
	}
	return GameController(c), nil
}

func (c GameController) Close() {
	sdlGameControllerClose(uintptr(c))
}

func (c GameController) Name() string {
	return sdlGameControllerName(uintptr(c))
}

// InstanceID returns the joystick instance ID, which SDL uses to name the
// controller in its events.
func (c GameController) InstanceID() int32 {
	return sdlJoystickInstanceID(sdlGameControllerGetJoy(uintptr(c)))
}

// GUID returns the SDL GUID of the controller as 32 hex digits.
func (c GameController) GUID() string {
	// The mapping starts with the GUID. Taking it from there avoids passing an
	// SDL_JoystickGUID struct by value.
	if p := sdlGameControllerMapping(uintptr(c)); p != nil {
		defer sdlFree(p)
		var guid []byte
		for i := 0; i < 64; i++ {
			b := *(*byte)(unsafe.Add(p, i))
			if b == 0 || b == ',' {
				break
			}
			guid = append(guid, b)
		}
		if len(guid) == 32 {
			return string(guid)
		}
	}

	// Build a USB GUID the way SDL does on Linux.
	le := func(v uint16) string {
		return fmt.Sprintf("%02x%02x", byte(v), byte(v>>8))
	}
	return "03000000" + le(sdlGameControllerGetVendor(uintptr(c))) + "0000" +
		le(sdlGameControllerGetProd(uintptr(c))) + "0000" +
		le(sdlGameControllerGetVer(uintptr(c))) + "0000"
}

// HasButton reports whether the controller's mapping has the button.
func (c GameController) HasButton(button int) bool {
	return sdlGameControllerHasButton(uintptr(c), int32(button)) != 0
}

// HasAxis reports whether the controller's mapping has the axis.
func (c GameController) HasAxis(axis int) bool {
	return sdlGameControllerHasAxis(uintptr(c), int32(axis)) != 0
}

// Button reports whether a button is pressed.
func (c GameController) Button(button int) bool {
	return sdlGameControllerGetButton(uintptr(c), int32(button)) != 0
}

// Axis returns an axis value in [-32768, 32767]. Triggers are in [0, 32767].
func (c GameController) Axis(axis int) int16 {
	return sdlGameControllerGetAxis(uintptr(c), int32(axis))
}

// Rumble starts a rumble, replacing the one in progress. It does nothing on a
// controller that cannot rumble.
func (c GameController) Rumble(low, high uint16, durationMS uint32) {
	sdlGameControllerRumble(uintptr(c), low, high, durationMS)
}
