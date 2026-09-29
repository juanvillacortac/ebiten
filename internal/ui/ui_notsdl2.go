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

//go:build !linux || android || nintendosdk || playstation5

package ui

import (
	"errors"
)

// sdl2Backend is never instantiated here. It exists so that Run can name the
// same type on every system.
type sdl2Backend struct{}

func sdl2BackendRequested() bool {
	return false
}

// maybeNewSDL2Backend never returns a backend: the SDL2 backend is for Linux.
func maybeNewSDL2Backend(u *UserInterface) (*sdl2Backend, error) {
	return nil, errors.New("ui: the SDL2 backend is a Linux feature")
}

func (b *sdl2Backend) run(game Game, options *RunOptions) error {
	return errors.New("ui: SDL2 is not available in this environment")
}
