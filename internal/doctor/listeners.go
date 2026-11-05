package doctor

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultListeners builds a "port → command line" map for listening TCP
// sockets by shelling out to lsof. Failure is not an error: an empty map makes
// the exposure check fall back to probing the port over HTTP.
func defaultListeners() map[int]string {
	out, err := runCmd("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpcn")
	if err != nil && len(out) == 0 {
		return map[int]string{}
	}
	pidToPorts, pidToName := parseLsofFields(string(out))

	// lsof -F gives the short command name; `ps` gives the full argv, which is
	// where "vite" and "--host" actually live.
	full := map[string]string{}
	if psOut, psErr := runCmd("ps", "-Ao", "pid=,command="); psErr == nil || len(psOut) > 0 {
		for _, line := range strings.Split(string(psOut), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			i := strings.IndexByte(line, ' ')
			if i <= 0 {
				continue
			}
			full[line[:i]] = strings.TrimSpace(line[i+1:])
		}
	}

	res := map[int]string{}
	for pid, ports := range pidToPorts {
		cmd := full[pid]
		if cmd == "" {
			cmd = pidToName[pid]
		}
		for _, p := range ports {
			if _, exists := res[p]; !exists {
				res[p] = cmd
			}
		}
	}
	return res
}

// parseLsofFields parses `lsof -F pcn` output: records begin with p<pid>,
// then c<command>, then one or more n<name> lines like "*:5173" or
// "127.0.0.1:18790".
func parseLsofFields(out string) (map[string][]int, map[string]string) {
	ports := map[string][]int{}
	names := map[string]string{}
	var pid string
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid = line[1:]
		case 'c':
			if pid != "" {
				names[pid] = line[1:]
			}
		case 'n':
			if pid == "" {
				continue
			}
			if p := portFromLsofName(line[1:]); p != 0 {
				ports[pid] = append(ports[pid], p)
			}
		}
	}
	return ports, names
}

// portFromLsofName pulls the listening port out of an lsof name field.
func portFromLsofName(name string) int {
	if i := strings.Index(name, "->"); i >= 0 {
		return 0 // an established connection, not a listener
	}
	i := strings.LastIndex(name, ":")
	if i < 0 {
		return 0
	}
	p, err := strconv.Atoi(strings.TrimSpace(name[i+1:]))
	if err != nil {
		return 0
	}
	return p
}

// httpGetBody fetches a URL and returns the first 8 KiB of the body.
var httpGetBody = func(url string) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return "", err
	}
	return string(body), nil
}
