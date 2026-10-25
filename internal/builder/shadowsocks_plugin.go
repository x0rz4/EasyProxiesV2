package builder

import (
	"errors"
	"sort"
	"strings"

	"easy_proxies/internal/nodecodec"

	"github.com/sagernet/sing-box/transport/sip003"
)

// Adapt Clash's simple-obfs dialect only at the runtime boundary. Normalizing
// the stored URI or Spec would change existing plugin nodes' identity hashes.
func shadowsocksRuntimePlugin(plugin nodecodec.Plugin) (nodecodec.Plugin, error) {
	if plugin.Name != "obfs" {
		return plugin, nil
	}
	args, err := sip003.ParsePluginOptions(plugin.Options)
	if err != nil {
		// The dependency's parse error can contain raw option values.
		return nodecodec.Plugin{}, errors.New("invalid obfs plugin options")
	}
	for alias, native := range map[string]string{"mode": "obfs", "host": "obfs-host"} {
		if values, ok := args[alias]; ok {
			if _, exists := args[native]; !exists {
				args[native] = values
			}
			delete(args, alias)
		}
	}
	keys := make([]string, 0, len(args))
	for key := range args {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	escape := strings.NewReplacer(`\`, `\\`, `;`, `\;`, `=`, `\=`)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		for _, value := range args[key] {
			parts = append(parts, escape.Replace(key)+"="+escape.Replace(value))
		}
	}
	return nodecodec.Plugin{Name: "obfs-local", Options: strings.Join(parts, ";")}, nil
}
