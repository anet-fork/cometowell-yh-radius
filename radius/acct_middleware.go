package radius

import (
	"encoding/binary"
	"go-rad/logger"
	"go-rad/model"
	"strconv"
	"time"
)

func AcctReply(cxt *Context) {
	cxt.Response.Code = AccountingResponseCode
	cxt.Response.PackageLength()
	secret := cxt.RadNas.Secret
	replyAuthenticator(cxt.Request.Authenticator, cxt.Response, secret)
	cxt.Listener.WriteToUDP(cxt.Response.ToByte(), cxt.Dst)
	cxt.Next()
}

func AcctRecord(cxt *Context) {
	attr, ok := cxt.Request.RadiusAttrStringKeyMap["Acct-Session-Id"]
	var acctSessionId string
	if ok {
		acctSessionId = attr.AttrStringValue
	}
	setAcctRecord(acctSessionId, cxt)
	cxt.Next()
}

func setAcctRecord(acctSessionId string, cxt *Context) {
	attr, _ := cxt.Request.RadiusAttrStringKeyMap["Acct-Status-Type"]
	statusType, _ := strconv.Atoi(attr.AttrStringValue)
	switch statusType {
	case AcctStatusTypeStart:
		acctStartHandler(acctSessionId, cxt)
	case AcctStatusTypeStop:
		acctStopHandler(acctSessionId, cxt)
	case AcctStatusTypeInterimUpdate:
		acctInterimUpdateHandler(acctSessionId, cxt)
	case AcctStatusTypeAccountingOn:
		go acctAccountingOn(cxt)
	case AcctStatusTypeAccountingOff:
		go acctAccountingOff(cxt)
	default:
		cxt.throwPackage = true
		panic("radius accounting status type is not supported")
	}
}

func acctStartHandler(acctSessionId string, cxt *Context) {
	online := model.RadOnlineUser{
		AcctSessionId: acctSessionId,
		NasIpAddr:     cxt.RadNas.IpAddr,
		StartTime:     model.NowTime(),
	}

	attr, ok := cxt.Request.RadiusAttrStringKeyMap["User-Name"]
	if ok {
		online.UserName = attr.AttrStringValue
	}

	attr, ok = cxt.Request.RadiusAttrStringKeyMap["Framed-IP-Address"]
	if ok {
		online.IpAddr = attr.AttrStringValue
	}

	attr, ok = cxt.Request.RadiusAttrStringKeyMap["NAS-Port-Id"]
	if ok {
		online.NasPortId = attr.AttrStringValue
	}

	online.MacAddr = getMacAddr(cxt)

	_, err := cxt.Session.InsertOne(&online)
	if err != nil {
		cxt.throwPackage = true
		panic("insert online user info failure" + err.Error())
	}
}

func acctStopHandler(acctSessionId string, cxt *Context) {

	online := model.RadOnlineUser{AcctSessionId: acctSessionId}
	cxt.Session.Get(&online)

	if online.Id == 0 {
		cxt.throwPackage = true
		panic("in online records can not find this: " + online.AcctSessionId)
	}

	// 单位KB
	var totalUpStream, totalDownStream int
	attr, ok := cxt.Request.RadiusAttrStringKeyMap["Acct-Input-Octets"]
	if ok {
		totalUpStream += int(binary.BigEndian.Uint32(attr.AttrValue)) / 1024
	}

	attr, ok = cxt.Request.RadiusAttrStringKeyMap["Acct-Output-Octets"]
	if ok {
		totalDownStream += int(binary.BigEndian.Uint32(attr.AttrValue)) / 1024
	}

	// This attribute indicates how many times the Acct-Input-Octets
	// counter has wrapped around 2^32 over the course of this service being provided
	attr, ok = cxt.Request.RadiusAttrStringKeyMap["Acct-Input-Gigawords"]
	if ok {
		totalUpStream += int(binary.BigEndian.Uint32(attr.AttrValue)) * 4 * 1024 * 1024
	}

	attr, ok = cxt.Request.RadiusAttrStringKeyMap["Acct-Input-Gigawords"]
	if ok {
		totalDownStream += int(binary.BigEndian.Uint32(attr.AttrValue)) * 4 * 1024 * 1024
	}

	accounting(online, totalUpStream, totalDownStream, cxt)
}

func accounting(online model.RadOnlineUser, totalUpStream int, totalDownStream int, cxt *Context) {
	// 添加online log
	now := time.Now()
	usedDuration := int(now.Sub(time.Time(online.StartTime)).Seconds())
	onlineLog := model.RadUserOnlineLog{
		UserName:        online.UserName,
		StartTime:       online.StartTime,
		StopTime:        model.NowTime(),
		UsedDuration:    usedDuration,
		TotalUpStream:   totalUpStream,
		TotalDownStream: totalDownStream,
		NasIpAddr:       online.NasIpAddr,
		IpAddr:          online.IpAddr,
		MacAddr:         online.MacAddr,
	}
	_, err := cxt.Session.InsertOne(&onlineLog)
	if err != nil {
		cxt.throwPackage = true
		panic("data access error")
	}

	// 扣除用户流量，时长
	user := model.RadUser{UserName: online.UserName}
	cxt.Session.Get(&user)
	user.AvailableFlow -= int64(totalDownStream) - int64(totalUpStream)
	user.AvailableTime -= int64(usedDuration)
	if user.AvailableFlow < 0 {
		user.AvailableFlow = 0
	}
	if user.AvailableTime < 0 {
		user.AvailableTime = 0
	}
	_, err = cxt.Session.Cols("available_flow", "available_time").Update(&user)
	if err != nil {
		cxt.throwPackage = true
		panic("data access error")
	}
	// 删除online
	delOnline := &model.RadOnlineUser{}
	_, err = cxt.Session.ID(online.Id).Delete(delOnline)
	if err != nil {
		cxt.throwPackage = true
		panic("data access error")
	}
}

func acctInterimUpdateHandler(acctSessionId string, cxt *Context) {
	online := model.RadOnlineUser{AcctSessionId: acctSessionId}
	cxt.Session.Get(&online)

	// 单位KB
	var totalUpStream, totalDownStream int
	attr, ok := cxt.Request.RadiusAttrStringKeyMap["Acct-Input-Octets"]
	if ok {
		totalUpStream += int(binary.BigEndian.Uint32(attr.AttrValue)) / 1024
	}

	attr, ok = cxt.Request.RadiusAttrStringKeyMap["Acct-Output-Octets"]
	if ok {
		totalDownStream += int(binary.BigEndian.Uint32(attr.AttrValue)) / 1024
	}

	// This attribute indicates how many times the Acct-Input-Octets
	// counter has wrapped around 2^32 over the course of this service being provided
	attr, ok = cxt.Request.RadiusAttrStringKeyMap["Acct-Input-Gigawords"]
	if ok {
		totalUpStream += int(binary.BigEndian.Uint32(attr.AttrValue)) * 4 * 1024 * 1024
	}

	attr, ok = cxt.Request.RadiusAttrStringKeyMap["Acct-Input-Gigawords"]
	if ok {
		totalDownStream += int(binary.BigEndian.Uint32(attr.AttrValue)) * 4 * 1024 * 1024
	}

	online.TotalUpStream += int64(totalUpStream)
	online.TotalUpStream += int64(totalUpStream)

	if online.Id == 0 {
		attr, ok := cxt.Request.RadiusAttrStringKeyMap["User-Name"]
		if !ok {
			cxt.throwPackage = true
			panic("Interim update: in online records can not find this account: " + acctSessionId)
		}
		online.UserName = attr.AttrStringValue
		var user model.RadUser
		cxt.Session.Where("username = ?", attr.AttrStringValue).Get(&user)
		if user.Id != 0 {
			online := model.RadOnlineUser{
				AcctSessionId: acctSessionId,
				NasIpAddr:     cxt.RadNas.IpAddr,
				StartTime:     model.NowTime(),
			}
			attr, ok = cxt.Request.RadiusAttrStringKeyMap["Framed-IP-Address"]
			if ok {
				online.IpAddr = attr.AttrStringValue
			}

			attr, ok = cxt.Request.RadiusAttrStringKeyMap["NAS-Port-Id"]
			if ok {
				online.NasPortId = attr.AttrStringValue
			}

			online.MacAddr = getMacAddr(cxt)
			_, err := cxt.Session.InsertOne(&online)
			if err != nil {
				cxt.throwPackage = true
				panic("Interim update: insert online user info failure" + err.Error())
			}
			return
		}
	}

	cxt.Session.ID(online.Id).Cols("total_up_stream", "total_down_stream").Update(&online)
}

// It may also be used to mark the start of accounting (for example, upon booting)
// by specifying Accounting-On and to mark the end of accounting
// (for example, just before a scheduled reboot) by specifying Accounting-Off.

// 计费开始，通常为设备重启后
func acctAccountingOn(cxt *Context) {
	onlineList := make([]model.RadOnlineUser, 0)
	cxt.Session.Find(&onlineList)
	offline(onlineList, cxt)
}

// 计费结束，通常为设备重启前
func acctAccountingOff(cxt *Context) {
	acctAccountingOn(cxt)
}

func offline(onlineList []model.RadOnlineUser, cxt *Context) {
	for _, online := range onlineList {
		accounting(online, int(online.TotalUpStream), int(online.TotalDownStream), cxt)
	}
	logger.Logger.Info("AccountingOn/AccountingOff下线处理完成")
}
