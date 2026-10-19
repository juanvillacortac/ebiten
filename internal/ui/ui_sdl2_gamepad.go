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
	"math"
	"time"

	"github.com/hajimehoshi/ebiten/v2/internal/gamepad"
	"github.com/hajimehoshi/ebiten/v2/internal/gamepaddb"
	"github.com/hajimehoshi/ebiten/v2/internal/sdl2"
)

// The SDL2 backend reads gamepads through SDL's game controller API, and hands
// them to Ebitengine as virtual gamepads. SDL applies the mappings it is given
// (SDL_GAMECONTROLLERCONFIG, as PortMaster sets per device), so the standard
// layout is the one every other SDL program on the device sees.

// sdl2Gamepads holds the open game controllers. It is used on the main thread
// only, except states, which the game thread reads after a poll returns.
type sdl2Gamepads struct {
	controllers []*sdl2Controller
	states      []gamepad.VirtualGamepadState
	vibrations  []gamepad.VirtualGamepadVibration

	// releasePending is set when the last snapshot showed a tap that had
	// already been released (see sdl2Controller.pressedSince): the next
	// snapshot shows the release, and a frame must see it, or the next tap of
	// the same button would not be a new press.
	releasePending bool
}

// sdl2Controller is an open game controller. Its ID is the gamepad ID the game
// sees, which is kept for as long as the controller is connected.
type sdl2Controller struct {
	controller sdl2.GameController
	instanceID int32
	id         gamepad.ID
	name       string
	guid       string

	axes    []float64
	buttons []bool
	// pressedSince records the buttons pressed since the last snapshot, and
	// triggerSince the furthest each trigger went: a press and its release
	// can both land between two snapshots (in FPSModeVsyncOffMinimum, polls can
	// be 250ms apart), and the snapshot alone would miss that tap. A latched
	// button reads pressed for one snapshot.
	pressedSince []bool
	triggerSince []float64
	// hasAxes and hasButtons report what the controller's mapping has.
	hasAxes    []bool
	hasButtons []bool

	standardAxes    map[gamepaddb.StandardAxis]float64
	standardButtons map[gamepaddb.StandardButton]gamepad.VirtualStandardGamepadButton
}

func newSDL2Gamepads() sdl2Gamepads {
	// A non-nil slice, even when empty, makes the gamepads virtual.
	return sdl2Gamepads{states: []gamepad.VirtualGamepadState{}}
}

// handleEvent handles a controller event, and reports whether ev was one.
func (g *sdl2Gamepads) handleEvent(ev *sdl2.Event) bool {
	switch ev.Type() {
	case sdl2.EventControllerDeviceAdded:
		g.open(int(ev.ControllerDevice()))
	case sdl2.EventControllerDeviceRemoved:
		g.close(ev.ControllerDevice())
	case sdl2.EventControllerDeviceRemapped:
		for _, c := range g.controllers {
			if c.instanceID == ev.ControllerDevice() {
				c.readMapping()
			}
		}
	case sdl2.EventControllerButtonDown:
		for _, c := range g.controllers {
			if b := ev.ControllerButton(); c.instanceID == ev.ControllerDevice() && b < len(c.pressedSince) {
				c.pressedSince[b] = true
			}
		}
	case sdl2.EventControllerAxisMotion:
		a := ev.ControllerAxis()
		if a != sdl2.ControllerAxisTriggerLeft && a != sdl2.ControllerAxisTriggerRight {
			return true
		}
		for _, c := range g.controllers {
			if c.instanceID == ev.ControllerDevice() {
				c.triggerSince[a] = max(c.triggerSince[a], float64(ev.ControllerAxisValue())/32767)
			}
		}
	default:
		return false
	}
	return true
}

func (g *sdl2Gamepads) open(deviceIndex int) {
	if !sdl2.IsGameController(deviceIndex) {
		return
	}
	gc, err := sdl2.GameControllerOpen(deviceIndex)
	if err != nil {
		return
	}
	instanceID := gc.InstanceID()
	for _, c := range g.controllers {
		if c.instanceID == instanceID {
			// Opening a controller again returns the same one with a new
			// reference.
			gc.Close()
			return
		}
	}

	// Take the smallest ID no connected controller has.
	var id gamepad.ID
	for {
		var taken bool
		for _, c := range g.controllers {
			if c.id == id {
				taken = true
				break
			}
		}
		if !taken {
			break
		}
		id++
	}

	c := &sdl2Controller{
		controller:      gc,
		instanceID:      instanceID,
		id:              id,
		name:            gc.Name(),
		guid:            gc.GUID(),
		axes:            make([]float64, sdl2.ControllerAxisCount),
		buttons:         make([]bool, sdl2.ControllerButtonCount),
		pressedSince:    make([]bool, sdl2.ControllerButtonCount),
		triggerSince:    make([]float64, sdl2.ControllerAxisCount),
		hasAxes:         make([]bool, sdl2.ControllerAxisCount),
		hasButtons:      make([]bool, sdl2.ControllerButtonCount),
		standardAxes:    map[gamepaddb.StandardAxis]float64{},
		standardButtons: map[gamepaddb.StandardButton]gamepad.VirtualStandardGamepadButton{},
	}
	c.readMapping()
	g.controllers = append(g.controllers, c)
}

// readMapping records which buttons and axes the controller's mapping has. A
// standard button or axis the mapping lacks is left out of the standard
// layout, so the game can tell it is unavailable.
func (c *sdl2Controller) readMapping() {
	for i := range c.hasAxes {
		c.hasAxes[i] = c.controller.HasAxis(i)
	}
	for i := range c.hasButtons {
		c.hasButtons[i] = c.controller.HasButton(i)
	}
	clear(c.standardAxes)
	clear(c.standardButtons)
}

func (g *sdl2Gamepads) close(instanceID int32) {
	for i, c := range g.controllers {
		if c.instanceID != instanceID {
			continue
		}
		c.controller.Close()
		g.controllers = append(g.controllers[:i], g.controllers[i+1:]...)
		return
	}
}

func (g *sdl2Gamepads) closeAll() {
	for _, c := range g.controllers {
		c.controller.Close()
	}
	g.controllers = nil
}

// rumble plays the vibrations the game asked for since the last poll.
func (g *sdl2Gamepads) rumble() {
	g.vibrations = gamepad.AppendVirtualGamepadVibrations(g.vibrations[:0])
	for _, v := range g.vibrations {
		for _, c := range g.controllers {
			if c.id != v.ID {
				continue
			}
			c.controller.Rumble(uint16(v.StrongMagnitude*math.MaxUint16), uint16(v.WeakMagnitude*math.MaxUint16), uint32(v.Duration/time.Millisecond))
		}
	}
}

// snapshot reads every controller's state into states. It must be called after
// the events are polled.
// snapshot reads the controllers, and reports whether a frame is needed even
// without events: to show the release of a tap that the previous snapshot
// latched.
func (g *sdl2Gamepads) snapshot() bool {
	needFrame := g.releasePending
	g.releasePending = false
	g.states = g.states[:0]
	for _, c := range g.controllers {
		if c.snapshot() {
			g.releasePending = true
		}
		g.states = append(g.states, gamepad.VirtualGamepadState{
			ID:              c.id,
			SDLID:           c.guid,
			Name:            c.name,
			Axes:            c.axes,
			Buttons:         c.buttons,
			StandardAxes:    c.standardAxes,
			StandardButtons: c.standardButtons,
		})
	}
	return needFrame
}

// sdl2StandardButtons maps the standard buttons to SDL's controller buttons.
// Both name the buttons by position. The triggers are axes in SDL and are
// handled apart.
var sdl2StandardButtons = [...]struct {
	standard gamepaddb.StandardButton
	sdl      int
}{
	{gamepaddb.StandardButtonRightBottom, sdl2.ControllerButtonA},
	{gamepaddb.StandardButtonRightRight, sdl2.ControllerButtonB},
	{gamepaddb.StandardButtonRightLeft, sdl2.ControllerButtonX},
	{gamepaddb.StandardButtonRightTop, sdl2.ControllerButtonY},
	{gamepaddb.StandardButtonFrontTopLeft, sdl2.ControllerButtonLeftShoulder},
	{gamepaddb.StandardButtonFrontTopRight, sdl2.ControllerButtonRightShoulder},
	{gamepaddb.StandardButtonCenterLeft, sdl2.ControllerButtonBack},
	{gamepaddb.StandardButtonCenterRight, sdl2.ControllerButtonStart},
	{gamepaddb.StandardButtonLeftStick, sdl2.ControllerButtonLeftStick},
	{gamepaddb.StandardButtonRightStick, sdl2.ControllerButtonRightStick},
	{gamepaddb.StandardButtonLeftTop, sdl2.ControllerButtonDPadUp},
	{gamepaddb.StandardButtonLeftBottom, sdl2.ControllerButtonDPadDown},
	{gamepaddb.StandardButtonLeftLeft, sdl2.ControllerButtonDPadLeft},
	{gamepaddb.StandardButtonLeftRight, sdl2.ControllerButtonDPadRight},
	{gamepaddb.StandardButtonCenterCenter, sdl2.ControllerButtonGuide},
}

// triggerPressThreshold is the value past which an analog trigger counts as
// pressed, as the standard gamepad of the Web defines.
const triggerPressThreshold = 30.0 / 255.0

// snapshot reads the controller, and reports whether it shows a tap that has
// already been released.
func (c *sdl2Controller) snapshot() bool {
	var latched bool
	for i := range c.axes {
		v := float64(c.controller.Axis(i)) / 32767
		// Only triggers are ever latched. A stick axis must not be compared
		// with its zero triggerSince: that would turn every negative value
		// (left, up) into 0, and report a latched tap on every snapshot.
		isTrigger := i == sdl2.ControllerAxisTriggerLeft || i == sdl2.ControllerAxisTriggerRight
		if isTrigger && c.triggerSince[i] > v {
			v, latched = c.triggerSince[i], true
		}
		c.triggerSince[i] = 0
		c.axes[i] = max(-1, min(1, v))
	}
	for i := range c.buttons {
		c.buttons[i] = c.controller.Button(i)
		if !c.buttons[i] && c.pressedSince[i] {
			c.buttons[i], latched = true, true
		}
		c.pressedSince[i] = false
	}

	for _, a := range [...]struct {
		standard gamepaddb.StandardAxis
		sdl      int
	}{
		{gamepaddb.StandardAxisLeftStickHorizontal, sdl2.ControllerAxisLeftX},
		{gamepaddb.StandardAxisLeftStickVertical, sdl2.ControllerAxisLeftY},
		{gamepaddb.StandardAxisRightStickHorizontal, sdl2.ControllerAxisRightX},
		{gamepaddb.StandardAxisRightStickVertical, sdl2.ControllerAxisRightY},
	} {
		if c.hasAxes[a.sdl] {
			c.standardAxes[a.standard] = c.axes[a.sdl]
		}
	}

	for _, m := range sdl2StandardButtons {
		if !c.hasButtons[m.sdl] {
			continue
		}
		pressed := c.buttons[m.sdl]
		var v float64
		if pressed {
			v = 1
		}
		c.standardButtons[m.standard] = gamepad.VirtualStandardGamepadButton{Pressed: pressed, Value: v}
	}
	// A trigger mapped to a digital button reads 0 or 1 through SDL, so digital
	// and analog triggers both land here.
	for _, t := range [...]struct {
		standard gamepaddb.StandardButton
		axis     int
	}{
		{gamepaddb.StandardButtonFrontBottomLeft, sdl2.ControllerAxisTriggerLeft},
		{gamepaddb.StandardButtonFrontBottomRight, sdl2.ControllerAxisTriggerRight},
	} {
		if !c.hasAxes[t.axis] {
			continue
		}
		v := max(0, c.axes[t.axis])
		c.standardButtons[t.standard] = gamepad.VirtualStandardGamepadButton{Pressed: v > triggerPressThreshold, Value: v}
	}
	return latched
}
