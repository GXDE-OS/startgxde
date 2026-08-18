/*
 * Copyright (C) 2017 ~ 2018 Deepin Technology Co., Ltd.
 *
 * Author:     jouyouyun <jouyouwen717@gmail.com>
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package xsettings

import (
	"fmt"
	"strconv"
	"time"
)

const (
	DPI_FALLBACK = 96
	HIDPI_LIMIT  = DPI_FALLBACK * 2

	ffKeyPixels = `user_pref("layout.css.devPixelsPerPx",`
)

// TODO: update 'antialias, hinting, hintstyle, rgba, cursor-theme, cursor-size'
func (m *XSManager) updateDPI() {
	scale := m.gs.GetDouble(gsKeyScaleFactor)
	if scale <= 0 {
		scale = 1
	}

	// Under Wayland the compositor (gxde-wlcom) opens the Xwayland root at the
	// maximum output scale and writes the matching Xft.dpi into the X resource
	// database. Use that scale so X11 clients render at the correct size on
	// fractional-scaled outputs even when the global "scale-factor" gsetting
	// (used for the integer Wayland path) is still 1.0.
	//
	// The compositor may publish Xft.dpi after startdde has started, so when it
	// is not available yet we retry for a short while instead of keeping the
	// 1.0 fallback (which would make X11 apps tiny on fractional outputs).
	if isWaylandSession() {
		if wscale := getXwaylandScale(); wscale > 0 {
			scale = wscale
		} else {
			m.retryXwaylandDPI()
			return
		}
	}

	var infos []xsSetting
	scaledDPI := int32(float64(DPI_FALLBACK*1024) * scale)
	if scaledDPI != m.gs.GetInt("xft-dpi") {
		m.gs.SetInt("xft-dpi", scaledDPI)
		infos = append(infos, xsSetting{
			sType: settingTypeInteger,
			prop:  "Xft/DPI",
			value: scaledDPI,
		})
	}

	// update window scale and cursor size
	windowScale := m.gs.GetInt(gsKeyWindowScale)
	if windowScale > 1 {
		scaledDPI = int32(DPI_FALLBACK * 1024)
	}
	cursorSize := m.gs.GetInt(gsKeyGtkCursorThemeSize)
	v, _ := m.GetInteger("Gdk/WindowScalingFactor")
	if v != windowScale {
		infos = append(infos, xsSetting{
			sType: settingTypeInteger,
			prop:  "Gdk/WindowScalingFactor",
			value: windowScale,
		}, xsSetting{
			sType: settingTypeInteger,
			prop:  "Gdk/UnscaledDPI",
			value: scaledDPI,
		}, xsSetting{
			sType: settingTypeInteger,
			prop:  "Gtk/CursorThemeSize",
			value: cursorSize,
		})
	}

	if len(infos) != 0 {
		err := m.setSettings(infos)
		if err != nil {
			logger.Warning("Failed to update dpi:", err)
		}
		m.updateXResources()
	}
}

// retryXwaylandDPI waits for the compositor to publish the Xwayland Xft.dpi
// into the X resource database and then recomputes the DPI once. It runs in its
// own goroutine so the synchronous updateDPI() call never blocks startup.
func (m *XSManager) retryXwaylandDPI() {
	go func() {
		const (
			delay   = 200 * time.Millisecond
			timeout = 8 * time.Second
		)
		deadline := time.Now().Add(timeout)
		for {
			time.Sleep(delay)
			if getXwaylandScale() > 0 {
				m.updateDPI()
				return
			}
			if time.Now().After(deadline) {
				logger.Warning("Wayland: Xwayland Xft.dpi still unavailable after timeout, keeping current DPI")
				return
			}
		}
	}()
}

func (m *XSManager) updateXResources() {
	scaleFactor := m.gs.GetDouble(gsKeyScaleFactor)
	xftDpi := int(DPI_FALLBACK * scaleFactor)
	updateXResources(xresourceInfos{
		&xresourceInfo{
			key:   "Xcursor.theme",
			value: m.gs.GetString("gtk-cursor-theme-name"),
		},
		&xresourceInfo{
			key:   "Xcursor.size",
			value: fmt.Sprintf("%d", m.gs.GetInt(gsKeyGtkCursorThemeSize)),
		},
		&xresourceInfo{
			key:   "Xft.dpi",
			value: strconv.Itoa(xftDpi),
		},
	})
}

