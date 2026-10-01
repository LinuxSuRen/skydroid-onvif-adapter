package onvif

import (
	"fmt"
	"strings"
	"time"
)

func registerDeviceOps() {
	reg(nsDevice, "GetServices", opGetServices)
	reg(nsDevice, "GetServiceCapabilities", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetServiceCapabilitiesResponse>` +
			`<tds:Capabilities Network="false" Security="false" System="false" IO="false"/>` +
			`</tds:GetServiceCapabilitiesResponse>`, nil
	})
	reg(nsDevice, "GetCapabilities", opGetCapabilities)
	reg(nsDevice, "GetDeviceInformation", opGetDeviceInformation)
	reg(nsDevice, "GetSystemDateAndTime", opGetSystemDateAndTime)
	reg(nsDevice, "GetHostname", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetHostnameResponse><tt:Hostname>skydroid-adapter.local</tt:Hostname></tds:GetHostnameResponse>`, nil
	})
	reg(nsDevice, "GetScopes", opGetScopes)
	reg(nsDevice, "GetNetworkInterfaces", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetNetworkInterfacesResponse/>`, nil
	})
	reg(nsDevice, "GetNetworkDefaultGateway", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetNetworkDefaultGatewayResponse/>`, nil
	})
	reg(nsDevice, "GetDNS", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetDNSResponse><tt:FromDHCP>false</tt:FromDHCP></tds:GetDNSResponse>`, nil
	})
	reg(nsDevice, "GetNTP", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetNTPResponse><tt:FromDHCP>false</tt:FromDHCP></tds:GetNTPResponse>`, nil
	})
	reg(nsDevice, "GetUsers", func(s *Service, _ []byte) (string, error) {
		return `<tds:GetUsersResponse><tds:User><tt:Username>admin</tt:Username><tt:UserLevel>Administrator</tt:UserLevel></tds:User></tds:GetUsersResponse>`, nil
	})
}

func serviceEntry(ns string) string {
	return fmt.Sprintf(`<tds:Service><tds:Namespace>%s</tds:Namespace>`, ns) +
		`<tds:XAddr>%XADDR%</tds:XAddr>` +
		`<tds:Version><tt:Major>2</tt:Major><tt:Minor>4</tt:Minor></tds:Version></tds:Service>`
}

func opGetServices(s *Service, _ []byte) (string, error) {
	x := xmlEsc(s.cfg.XAddr())
	body := `<tds:GetServicesResponse>` +
		strings.ReplaceAll(serviceEntry(nsDevice), "%XADDR%", x) +
		strings.ReplaceAll(serviceEntry(nsMedia), "%XADDR%", x) +
		strings.ReplaceAll(serviceEntry(nsPTZ), "%XADDR%", x) +
		`</tds:GetServicesResponse>`
	return body, nil
}

func opGetCapabilities(s *Service, _ []byte) (string, error) {
	x := xmlEsc(s.cfg.XAddr())
	body := `<tds:GetCapabilitiesResponse><tds:Capabilities>` +
		`<tt:Device><tt:XAddr>` + x + `</tt:XAddr>` +
		`<tt:Network><tt:IPFilter>false</tt:IPFilter><tt:ZeroConfiguration>false</tt:ZeroConfiguration>` +
		`<tt:IPVersion6>false</tt:IPVersion6><tt:DynDNS>false</tt:DynDNS></tt:Network>` +
		`<tt:System></tt:System><tt:Security></tt:Security></tt:Device>` +
		`<tt:Media><tt:XAddr>` + x + `</tt:XAddr>` +
		`<tt:StreamingCapabilities><tt:RTPMulticast>false</tt:RTPMulticast><tt:RTP_TCP>true</tt:RTP_TCP>` +
		`<tt:RTP_RTSP_TCP>true</tt:RTP_RTSP_TCP></tt:StreamingCapabilities></tt:Media>` +
		`<tt:PTZ><tt:XAddr>` + x + `</tt:XAddr></tt:PTZ>` +
		`</tds:Capabilities></tds:GetCapabilitiesResponse>`
	return body, nil
}

func opGetDeviceInformation(s *Service, _ []byte) (string, error) {
	fw := s.cfg.Firmware
	if fw == "" {
		fw = "unknown"
	}
	body := `<tds:GetDeviceInformationResponse>` +
		`<tds:Manufacturer>` + xmlEsc(s.cfg.Manufacturer) + `</tds:Manufacturer>` +
		`<tds:Model>` + xmlEsc(s.cfg.Model) + `</tds:Model>` +
		`<tds:FirmwareVersion>` + xmlEsc(fw) + `</tds:FirmwareVersion>` +
		`<tds:SerialNumber>` + xmlEsc(s.cfg.Serial) + `</tds:SerialNumber>` +
		`<tds:HardwareId>skydroid-tp-bridge</tds:HardwareId>` +
		`</tds:GetDeviceInformationResponse>`
	return body, nil
}

func opGetSystemDateAndTime(_ *Service, _ []byte) (string, error) {
	dt := dateTimeXML(time.Now().UTC())
	local := dateTimeXML(time.Now())
	return `<tds:GetSystemDateAndTimeResponse><tds:SystemDateAndTime>` +
		`<tt:DateTimeType>Manual</tt:DateTimeType><tt:DaySavings>PT0S</tt:DaySavings><tt:TimeZone>UTC</tt:TimeZone>` +
		`<tt:UTCDateTime>` + dt + `</tt:UTCDateTime>` +
		`<tt:LocalDateTime>` + local + `</tt:LocalDateTime>` +
		`</tds:SystemDateAndTime></tds:GetSystemDateAndTimeResponse>`, nil
}

func dateTimeXML(t time.Time) string {
	return fmt.Sprintf(`<tt:Time><tt:Hour>%d</tt:Hour><tt:Minute>%d</tt:Minute><tt:Second>%d</tt:Second></tt:Time>`+
		`<tt:Date><tt:Year>%d</tt:Year><tt:Month>%d</tt:Month><tt:Day>%d</tt:Day></tt:Date>`,
		t.Hour(), t.Minute(), t.Second(), t.Year(), int(t.Month()), t.Day())
}

func opGetScopes(s *Service, _ []byte) (string, error) {
	scopes := []string{
		"onvif://www.onvif.org/type/NetworkVideoTransmitter",
		"onvif.org/type/ptz",
		"onvif://www.onvif.org/Profile/Streaming",
		"onvif://www.onvif.org/name/Skydroid " + s.cfg.Model,
	}
	body := `<tds:GetScopesResponse>`
	for _, sc := range scopes {
		body += `<tds:Scopes><tt:ScopeDefinition>Fixed</tt:ScopeDefinition><tt:ScopeItem>` + xmlEsc(sc) + `</tt:ScopeItem></tds:Scopes>`
	}
	return body + `</tds:GetScopesResponse>`, nil
}
