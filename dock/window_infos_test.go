/*
 * Copyright (C) 2014 ~ 2018 Deepin Technology Co., Ltd.
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

package dock

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func Test_windowInfosTypeEqual(t *testing.T) {
	Convey("windowInfosType Equal", t, func() {
		a := windowInfosType{
			0: {"a", false},
			1: {"b", false},
			2: {"c", true},
		}
		b := windowInfosType{
			2: {"c", true},
			1: {"b", false},
			0: {"a", false},
		}
		So(a.Equal(b), ShouldBeTrue)

		c := windowInfosType{
			1: {"b", false},
			2: {"c", false},
		}
		So(c.Equal(a), ShouldBeFalse)

		d := windowInfosType{
			0: {"aa", false},
			1: {"b", false},
			2: {"c", false},
		}
		So(d.Equal(a), ShouldBeFalse)

		e := windowInfosType{
			0: {"a", false},
			1: {"b", false},
			3: {"c", false},
		}
		So(e.Equal(a), ShouldBeFalse)

		f := windowInfosType{
			0: {"a", false},
			1: {"b", false},
			2: {"c", false},
		}
		So(f.Equal(a), ShouldBeFalse)
	})
}
