package onvif

import (
	"encoding/xml"
	"errors"
	"fmt"
)

func registerMediaOps() {
	reg(nsMedia, "GetProfiles", opGetProfiles)
	reg(nsMedia, "GetProfile", withProfile(opGetProfile))
	reg(nsMedia, "GetVideoSources", opGetVideoSources)
	reg(nsMedia, "GetVideoEncoderConfigurations", opGetEncoderConfigs)
	reg(nsMedia, "GetVideoEncoderConfiguration", withEncoderToken(func(s *Service, i int) (string, error) {
		p := s.cfg.Profiles[i]
		return `<trt:GetVideoEncoderConfigurationResponse>` + encoderCfgXML(p) + `</trt:GetVideoEncoderConfigurationResponse>`, nil
	}))
	reg(nsMedia, "GetVideoEncoderConfigurationOptions", withProfile(func(s *Service, p Profile) (string, error) {
		res := func(w, h int) string {
			return fmt.Sprintf(`<tt:ResolutionsAvailable><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:ResolutionsAvailable>`, w, h)
		}
		body := `<trt:GetVideoEncoderConfigurationOptionsResponse><trt:Options>` +
			`<tt:QualityRange><tt:Min>1</tt:Min><tt:Max>10</tt:Max></tt:QualityRange>` +
			res(p.Width, p.Height) + res(1920, 1080) + res(1280, 720) + res(640, 360) +
			`<tt:H264Options>` + res(1920, 1080) + res(1280, 720) + res(640, 360) +
			`<tt:GovLengthRange><tt:Min>1</tt:Min><tt:Max>120</tt:Max></tt:GovLengthRange>` +
			`<tt:FrameRateRange><tt:Min>1</tt:Min><tt:Max>60</tt:Max></tt:FrameRateRange>` +
			`<tt:EncodingProfiles>Main</tt:EncodingProfiles></tt:H264Options>` +
			`</trt:Options></trt:GetVideoEncoderConfigurationOptionsResponse>`
		return body, nil
	}))
	reg(nsMedia, "GetStreamUri", withProfile(opGetStreamUri))
	reg(nsMedia, "GetSnapshotUri", withProfile(opGetSnapshotUri))
	reg(nsMedia, "GetAudioSources", func(s *Service, _ []byte) (string, error) {
		return `<trt:GetAudioSourcesResponse/>`, nil
	})
	reg(nsMedia, "GetServiceCapabilities", func(s *Service, _ []byte) (string, error) {
		return `<trt:GetServiceCapabilitiesResponse>` +
			`<trt:Capabilities SnapshotUri="true" Rotation="false" OSD="false"/>` +
			`</trt:GetServiceCapabilitiesResponse>`, nil
	})
}

func opGetProfiles(s *Service, _ []byte) (string, error) {
	body := `<trt:GetProfilesResponse>`
	for _, p := range s.cfg.Profiles {
		body += profileXML(p)
	}
	return body + `</trt:GetProfilesResponse>`, nil
}

func opGetProfile(s *Service, p Profile) (string, error) {
	return `<trt:GetProfileResponse>` + profileXML(p) + `</trt:GetProfileResponse>`, nil
}

func profileXML(p Profile) string {
	return `<trt:Profiles token="` + p.Token + `" fixed="true">` +
		`<tt:Name>` + xmlEsc(p.Name) + `</tt:Name>` +
		`<trt:VideoSourceConfiguration token="vsc_` + p.Token + `">` +
		`<tt:Name>` + xmlEsc(p.Name) + `</tt:Name><tt:UseCount>1</tt:UseCount>` +
		`<tt:SourceToken>vs_main</tt:SourceToken>` +
		fmt.Sprintf(`<tt:Bounds x="0" y="0" width="%d" height="%d"/></trt:VideoSourceConfiguration>`, p.Width, p.Height) +
		encoderCfgXML(p) +
		ptzCfgXML() +
		`</trt:Profiles>`
}

func encoderCfgXML(p Profile) string {
	gov := p.Framerate * 2
	if gov < 10 {
		gov = 10
	}
	return `<trt:VideoEncoderConfiguration token="vec_` + p.Token + `">` +
		`<tt:Name>H264</tt:Name><tt:UseCount>1</tt:UseCount>` +
		`<tt:Encoding>H264</tt:Encoding>` +
		fmt.Sprintf(`<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>`, p.Width, p.Height) +
		`<tt:Quality>5</tt:Quality>` +
		fmt.Sprintf(`<tt:RateControl><tt:FrameRateLimit>%d</tt:FrameRateLimit><tt:EncodingInterval>1</tt:EncodingInterval><tt:BitrateLimit>4096</tt:BitrateLimit></tt:RateControl>`, p.Framerate) +
		fmt.Sprintf(`<tt:H264><tt:GovLength>%d</tt:GovLength><tt:H264Profile>Main</tt:H264Profile></tt:H264>`, gov) +
		`<tt:SessionTimeout>PT60S</tt:SessionTimeout>` +
		`</trt:VideoEncoderConfiguration>`
}

func opGetEncoderConfigs(s *Service, _ []byte) (string, error) {
	body := `<trt:GetVideoEncoderConfigurationsResponse>`
	for _, p := range s.cfg.Profiles {
		body += encoderCfgXML(p)
	}
	return body + `</trt:GetVideoEncoderConfigurationsResponse>`, nil
}

func opGetVideoSources(s *Service, _ []byte) (string, error) {
	main := s.cfg.Profiles[0]
	body := `<trt:GetVideoSourcesResponse><trt:VideoSources token="vs_main">` +
		fmt.Sprintf(`<tt:Framerate>%d</tt:Framerate>`, main.Framerate) +
		fmt.Sprintf(`<tt:Resolution><tt:Width>%d</tt:Width><tt:Height>%d</tt:Height></tt:Resolution>`, main.Width, main.Height) +
		`</trt:VideoSources></trt:GetVideoSourcesResponse>`
	return body, nil
}

func opGetStreamUri(s *Service, p Profile) (string, error) {
	uri := xmlEsc(s.cfg.StreamURI(p))
	return `<trt:GetStreamUriResponse><trt:MediaUri>` +
		`<tt:Uri>` + uri + `</tt:Uri>` +
		`<tt:InvalidAfterConnect>false</tt:InvalidAfterConnect>` +
		`<tt:InvalidAfterReboot>false</tt:InvalidAfterReboot>` +
		`<tt:Timeout>PT60S</tt:Timeout>` +
		`</trt:MediaUri></trt:GetStreamUriResponse>`, nil
}

func opGetSnapshotUri(s *Service, p Profile) (string, error) {
	uri := xmlEsc(s.cfg.SnapshotURI(p))
	return `<trt:GetSnapshotUriResponse><trt:MediaUri>` +
		`<tt:Uri>` + uri + `</tt:Uri>` +
		`<tt:InvalidAfterConnect>false</tt:InvalidAfterConnect>` +
		`<tt:InvalidAfterReboot>false</tt:InvalidAfterReboot>` +
		`<tt:Timeout>PT10S</tt:Timeout>` +
		`</trt:MediaUri></trt:GetSnapshotUriResponse>`, nil
}

// ---- 请求解析 ----

func parseInner(inner []byte, v interface{}) error {
	if err := xml.Unmarshal(inner, v); err != nil {
		return fmt.Errorf("parse request: %w", err)
	}
	return nil
}

func withProfile(fn func(s *Service, p Profile) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req struct {
			ProfileToken string `xml:"ProfileToken"`
		}
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		p, ok := s.profile(req.ProfileToken)
		if !ok {
			return "", fmt.Errorf("profile %q not found", req.ProfileToken)
		}
		return fn(s, p)
	}
}

func withEncoderToken(fn func(s *Service, index int) (string, error)) opHandler {
	return func(s *Service, inner []byte) (string, error) {
		var req struct {
			ConfigurationToken string `xml:"ConfigurationToken"`
		}
		if err := parseInner(inner, &req); err != nil {
			return "", err
		}
		for i, p := range s.cfg.Profiles {
			if "vec_"+p.Token == req.ConfigurationToken {
				return fn(s, i)
			}
		}
		return "", errors.New("encoder configuration not found")
	}
}
