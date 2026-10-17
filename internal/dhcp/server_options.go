package dhcp

import (
	"encoding/binary"
	"net"
	"strings"

	"github.com/insomniacslk/dhcp/dhcpv4"
)

func (s *Server) advancedOptions(request, reply *dhcpv4.DHCPv4, options map[byte]dhcpv4.Option) {
	put := func(code dhcpv4.OptionCode, value []byte) {
		if len(value) != 0 {
			options[code.Code()] = dhcpv4.OptGeneric(code, value)
		}
	}
	var ntp []byte
	for _, server := range s.cfg.NTPServers {
		ntp = append(ntp, server.AsSlice()...)
	}
	put(dhcpv4.OptionNTPServers, ntp)
	put(dhcpv4.OptionDNSDomainSearchList, encodeSearchDomains(s.cfg.DomainSearch))
	routes := encodeClasslessRoutes(s.cfg.ClasslessRoutes)
	put(dhcpv4.OptionClasslessStaticRoute, routes)
	put(dhcpv4.GenericOptionCode(249), routes)
	boot := BootProfile{BootServer: s.cfg.BootServer, BootServerName: s.cfg.BootServerName, BootFile: s.cfg.BootFile}
	architectures := request.Options.Get(dhcpv4.OptionClientSystemArchitectureType)
match:
	for len(architectures) >= 2 {
		architecture := binary.BigEndian.Uint16(architectures[:2])
		architectures = architectures[2:]
		for _, profile := range s.cfg.BootProfiles {
			if profile.Architecture == architecture {
				boot = profile
				break match
			}
		}
	}
	if boot.BootServer.IsValid() {
		reply.ServerIPAddr = net.IP(boot.BootServer.AsSlice())
	}
	reply.BootFileName = boot.BootFile
	// sname is only 64 bytes. Longer names are carried intact in option 66.
	if len(boot.BootServerName) <= 63 {
		reply.ServerHostName = boot.BootServerName
	}
	put(dhcpv4.OptionTFTPServerName, []byte(boot.BootServerName))
	put(dhcpv4.OptionBootfileName, []byte(boot.BootFile))
}

// clientFQDN parses RFC 4702's DNS labels or deprecated ASCII representation.
// Compression is forbidden here. Advisory names never authorize DNS UPDATE.
func clientFQDN(value []byte) (string, bool) {
	if len(value) < 3 || len(value) > 255 || value[0]&0xf2 != 0 || value[0]&9 == 9 {
		return "", false
	}
	data := value[3:]
	if len(data) == 0 {
		return "", true
	}
	if value[0]&4 == 0 {
		name := strings.TrimSuffix(string(data), ".")
		return name, name != "" && validHostname(name)
	}
	var labels []string
	for len(data) > 0 {
		length := int(data[0])
		data = data[1:]
		if length == 0 {
			if len(data) != 0 {
				return "", false
			}
			name := strings.Join(labels, ".")
			return name, validHostname(name)
		}
		if length > 63 || length > len(data) {
			return "", false
		}
		label := string(data[:length])
		if strings.Contains(label, ".") || !validHostname(label) {
			return "", false
		}
		labels = append(labels, label)
		data = data[length:]
	}
	// A non-terminated sequence is a legal RFC 4702 partial domain name.
	name := strings.Join(labels, ".")
	return name, name != "" && validHostname(name)
}

func fqdnReply(value []byte) []byte {
	response := append([]byte(nil), value...)
	response[0] = value[0]&4 | 8 // E retained, N=1: no server DNS UPDATE.
	if value[0]&1 != 0 {
		response[0] |= 2
	} // O: override client's requested S bit.
	response[1], response[2] = 0, 0
	return response
}
