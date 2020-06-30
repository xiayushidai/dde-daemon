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

package network

import (
	"sync"
	"time"

	sysNetwork "github.com/linuxdeepin/go-dbus-factory/com.deepin.system.network"
	nmdbus "github.com/linuxdeepin/go-dbus-factory/org.freedesktop.networkmanager"
	secrets "github.com/linuxdeepin/go-dbus-factory/org.freedesktop.secrets"
	"pkg.deepin.io/dde/daemon/common/dsync"
	"pkg.deepin.io/dde/daemon/network/proxychains"
	"pkg.deepin.io/dde/daemon/session/common"
	dbus "pkg.deepin.io/lib/dbus1"
	"pkg.deepin.io/lib/dbusutil"
	"pkg.deepin.io/lib/dbusutil/proxy"
	"pkg.deepin.io/lib/strv"
)

const (
	dbusServiceName = "com.deepin.daemon.Network"
	dbusPath        = "/com/deepin/daemon/Network"
	dbusInterface   = "com.deepin.daemon.Network"
)

type connectionData map[string]map[string]dbus.Variant

//go:generate dbusutil-gen -type Manager manager.go

// Manager is the main DBus object for network module.
type Manager struct {
	sysSigLoop   *dbusutil.SignalLoop
	service      *dbusutil.Service
	sysNetwork   *sysNetwork.Network
	nmObjManager *nmdbus.ObjectManager
	PropsMu      sync.RWMutex
	// update by manager.go
	State        uint32 // global networking state
	Connectivity uint32

	NetworkingEnabled bool `prop:"access:rw"` // airplane mode for NetworkManager
	VpnEnabled        bool `prop:"access:rw"`

	// hidden properties
	wirelessEnabled bool
	wwanEnabled     bool
	wiredEnabled    bool

	// update by manager_devices.go
	devicesLock sync.Mutex
	devices     map[string][]*device
	Devices     string // array of device objects and marshaled by json

	accessPointsLock sync.Mutex
	accessPoints     map[dbus.ObjectPath][]*accessPoint

	// update by manager_connections.go
	connectionsLock sync.Mutex
	connections     map[string]connectionSlice
	Connections     string // array of connection information and marshaled by json

	// update by manager_active.go
	activeConnectionsLock sync.Mutex
	activeConnections     map[dbus.ObjectPath]*activeConnection
	ActiveConnections     string // array of connections that activated and marshaled by json

	secretAgent        *SecretAgent
	stateHandler       *stateHandler
	proxyChainsManager *proxychains.Manager

	canNotify			bool //do not notify "disconected" when Unconnect wifi
	hasSaveSecret 		bool //determine whether to save the password to the keyring
	items 				[]settingItem // save password information temporarily
	
	sessionSigLoop *dbusutil.SignalLoop
	syncConfig     *dsync.Config

	ActiveConnectDevpath 	 dbus.ObjectPath
	ActiveConnectUuid 		 string
	ActiveConnectSettingPath dbus.ObjectPath

	signals *struct {
		AccessPointAdded, AccessPointRemoved, AccessPointPropertiesChanged struct {
			devPath, apJSON string
		}
		DeviceEnabled struct {
			devPath string
			enabled bool
		}
	}

	methods *struct {
		ActivateAccessPoint          func() `in:"uuid,apPath,devPath" out:"cPath"`
		ActivateConnection           func() `in:"uuid,devPath" out:"cPath"`
		DeactivateConnection         func() `in:"uuid"`
		DeleteConnection             func() `in:"uuid"`
		DisableWirelessHotspotMode   func() `in:"devPath"`
		DisconnectDevice             func() `in:"devPath"`
		EnableDevice                 func() `in:"devPath,enabled"`
		EnableWirelessHotspotMode    func() `in:"devPath"`
		GetAccessPoints              func() `in:"path" out:"apsJSON"`
		GetActiveConnectionInfo      func() `out:"acInfosJSON"`
		GetAutoProxy                 func() `out:"proxyAuto"`
		GetProxy                     func() `in:"proxyType" out:"host,port"`
		GetProxyIgnoreHosts          func() `out:"ignoreHosts"`
		GetProxyMethod               func() `out:"proxyMode"`
		GetSupportedConnectionTypes  func() `out:"types"`
		IsDeviceEnabled              func() `in:"devPath" out:"enabled"`
		IsWirelessHotspotModeEnabled func() `in:"devPath" out:"enabled"`
		ListDeviceConnections        func() `in:"devPath" out:"connections"`
		SetAutoProxy                 func() `in:"proxyAuto"`
		SetDeviceManaged             func() `in:"devPathOrIfc,managed"`
		SetProxy                     func() `in:"proxyType,host,port"`
		SetProxyIgnoreHosts          func() `in:"ignoreHosts"`
		SetProxyMethod               func() `in:"proxyMode"`
	}
}

func (*Manager) GetInterfaceName() string {
	return dbusInterface
}

// initialize slice code manually to make i18n works
func initSlices() {
	initProxyGsettings()
	initNmStateReasons()
}

func NewManager(service *dbusutil.Service) (m *Manager) {
	m = &Manager{
		service: service,
	}
	return
}

func (m *Manager) init() {
	logger.Info("initialize network")  //初始化网络

	systemBus, err := dbus.SystemBus() //和系统bus进行连接
	if err != nil {
		return
	}

	sessionBus, err := dbus.SessionBus() //和session bus进行连接
	if err != nil {
		return
	}

	m.sysSigLoop = sysSigLoop
	m.initDbusObjects()  //初始化各种dbus的obj

	disableNotify()	//禁用通知
	defer enableNotify()  //启用通知,执行该函数后,在五秒后启用通知.

	sysService, err := dbusutil.NewSystemService()
	if err != nil {
		logger.Warning(err)
		return
	}

	// TODO(jouyouyun): improve in future
	// Sometimes the 'org.freedesktop.secrets' is not exists, this would block the 'init' function, so move to goroutinue
	go func() {  //开始一个协程,防止阻塞.
		secServiceObj := secrets.NewService(sessionBus)  //secrets的对象
		sa, err := newSecretAgent(secServiceObj,m)
		if err != nil {
			logger.Warning(err)
			return
		}
		m.secretAgent = sa

		logger.Debug("unique name on system bus:", systemBus.Names()[0])
		err = sysService.Export("/org/freedesktop/NetworkManager/SecretAgent", sa)
		if err != nil {
			logger.Warning(err)
			return
		}

		// register secret agent
		nmAgentManager := nmdbus.NewAgentManager(systemBus)
		err = nmAgentManager.Register(0, "com.deepin.daemon.network.SecretAgent")
		if err != nil {
			logger.Debug("failed to register secret agent:", err)
		} else {
			logger.Debug("register secret agent ok")
		}
	}()

	// initialize device and connection handlers

	m.initConnectionManage()  //初始化,监听连接的变化
	m.initDeviceManage()    //初始化,监听设备的变化
	m.initActiveConnectionManage()	//初始化,监听active和vpn的state状态变化.
	m.initNMObjManager(systemBus)  //增加和删除接口.
	m.initSysNetwork(systemBus)

	m.stateHandler = newStateHandler(m.sysSigLoop, m)

	// update property "State"
	err = nmManager.State().ConnectChanged(func(hasValue bool, value uint32) {  //连接发生变化.
		m.updatePropState()
	})
	if err != nil {
		logger.Warning(err)
	}
	m.updatePropState() //更新状态.

	// update property Connectivity
	_ = nmManager.Connectivity().ConnectChanged(func(hasValue bool, value uint32) {
		m.updatePropConnectivity()
	})
	m.updatePropConnectivity()

	// move to power module
	// connect computer suspend signal
	// _, err = loginManager.ConnectPrepareForSleep(func(active bool) {
	// 	if active {
	// 		// suspend
	// 		disableNotify()
	// 	} else {
	// 		// restore
	// 		enableNotify()

	// 		_ = m.RequestWirelessScan()
	// 	}
	// })
	// if err != nil {
	// 	logger.Warning(err)
	// }

	m.sessionSigLoop = dbusutil.NewSignalLoop(m.service.Conn(), 10)
	logger.Warning("ths is init start")
	m.sessionSigLoop.Start()	//bug#26967
	m.syncConfig = dsync.NewConfig("network", &syncConfig{m: m},   //云同步config.不知道什么意思.难道这个是同步不同用户之间的信息.
		m.sessionSigLoop, dbusPath, logger)
}

func (m *Manager) destroy() {
	logger.Info("destroy network")
	m.sessionSigLoop.Stop()
	m.syncConfig.Destroy()
	m.nmObjManager.RemoveHandler(proxy.RemoveAllHandlers)
	m.sysNetwork.RemoveHandler(proxy.RemoveAllHandlers)
	destroyDbusObjects()
	destroyStateHandler(m.stateHandler)
	m.clearDevices()
	m.clearAccessPoints()
	m.clearConnections()
	m.clearActiveConnections()

	// reset dbus properties
	m.setPropNetworkingEnabled(false)
	m.updatePropState()
}

func watchNetworkManagerRestart(m *Manager) {
	_, err := dbusDaemon.ConnectNameOwnerChanged(func(name, oldOwner, newOwner string) {
		if name == "org.freedesktop.NetworkManager" {
			// if a new dbus session was installed, the name and newOwner
			// will be no empty, if a dbus session was uninstalled, the
			// name and oldOwner will be not empty
			if len(newOwner) != 0 {
				// network-manager is starting
				logger.Info("network-manager is starting")
				time.Sleep(1 * time.Second)
				m.init()
			} else {
				// network-manager stopped
				logger.Info("network-manager stopped")
				m.destroy()
			}
		}
	})
	if err != nil {
		logger.Warning(err)
	}
}

func (m *Manager) initSysNetwork(sysBus *dbus.Conn) {
	m.sysNetwork = sysNetwork.NewNetwork(sysBus)
	m.sysNetwork.InitSignalExt(m.sysSigLoop, true)
	err := common.ActivateSysDaemonService(m.sysNetwork.ServiceName_())
	if err != nil {
		logger.Warning(err)
	}

	_, err = m.sysNetwork.ConnectDeviceEnabled(func(devPath dbus.ObjectPath, enabled bool) {
		err := m.service.Emit(manager, "DeviceEnabled", string(devPath), enabled)
		if err != nil {
			logger.Warning(err)
		}
	})
	if err != nil {
		logger.Warning(err)
	}

	vpnEnabled, err := m.sysNetwork.VpnEnabled().Get(0)
	if err != nil {
		logger.Warning(err)
	} else {
		m.VpnEnabled = vpnEnabled
	}

	err = m.sysNetwork.VpnEnabled().ConnectChanged(func(hasValue bool, value bool) {
		if !hasValue {
			return
		}

		m.PropsMu.Lock()
		m.setPropVpnEnabled(value)
		m.PropsMu.Unlock()
	})
	if err != nil {
		logger.Warning(err)
	}
}

func (m *Manager) initNMObjManager(systemBus *dbus.Conn) {
	objManager := nmdbus.NewObjectManager(systemBus)
	m.nmObjManager = objManager
	objManager.InitSignalExt(m.sysSigLoop, true)
	_, err := objManager.ConnectInterfacesAdded(func(objectPath dbus.ObjectPath,
		interfacesAndProperties map[string]map[string]dbus.Variant) { //func是一个匿名的回调函数,如果有InterfaceAdd信号,触发该回调.
		_, ok := interfacesAndProperties["org.freedesktop.NetworkManager.Connection.Active"]
		if ok {
			// add active connection
			m.activeConnectionsLock.Lock()
			defer m.activeConnectionsLock.Unlock()

			logger.Debug("add active connection", objectPath)
			aConn := m.newActiveConnection(objectPath)
			m.activeConnections[objectPath] = aConn
			m.updatePropActiveConnections()
		}
	})
	if err != nil {
		logger.Warning(err)
	}
	_, err = objManager.ConnectInterfacesRemoved(func(objectPath dbus.ObjectPath, interfaces []string) {
		if strv.Strv(interfaces).Contains("org.freedesktop.NetworkManager.Connection.Active") {  //如果有InterfaceRemoved信号.触发该回调.
			// remove active connection
			m.activeConnectionsLock.Lock()
			defer m.activeConnectionsLock.Unlock()

			logger.Debug("remove active connection", objectPath)
			delete(m.activeConnections, objectPath)
			m.updatePropActiveConnections()
		}
	})
	if err != nil {
		logger.Warning(err)
	}
}
