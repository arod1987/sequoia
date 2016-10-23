package sequoia

/* Scope.go
 *
 * Reads in scope spec file and defines
 * methods to configure scope.
 *
 * The scope Object includes a container manager
 * for creating containers required for setup
 * and a reference to Provider that offers
 * couchbase resources.
 *
 */

import (
	"fmt"
	"github.com/streamrail/concurrent-map"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type Scope struct {
	Spec     ScopeSpec
	Cm       *ContainerManager
	Provider Provider
	Flags    TestFlags
	Version  string
	Vars     cmap.ConcurrentMap
	Loops    int
}

func ApplyOverrides(overrides string, spec *ScopeSpec) {
	parts := strings.Split(overrides, ",")
	for _, component := range parts {
		subparts := strings.Split(component, ":")
		if len(subparts) > 2 {
			// rejoin if already had colon
			subparts = []string{subparts[0],
				strings.Join(subparts[1:], ":"),
			}
		}
		if len(subparts) == 2 {
			key := subparts[0]
			vals := strings.Split(subparts[1], ".")

			switch key {
			case "servers": // server override
				for i, server := range spec.Servers {
					if server.Name == vals[0] {
						attrs := strings.Split(vals[1], "=")
						_k := ToCamelCase(attrs[0])
						_v := attrs[1]

						// reflect to spec field
						rspec := reflect.ValueOf(&server)
						el := rspec.Elem()
						val := el.FieldByName(_k)
						switch val.Kind() {
						case reflect.Uint8:
							// update fuild as uint
							u, _ := strconv.ParseUint(_v, 10, 8)
							val.SetUint(u)
						case reflect.String:
							// update fuild as string
							val.SetString(_v)
						}
						spec.Servers[i] = server
					}
				}
			} // TODO: add in bucket, ddoc overrides
		}
	}

}

func NewScope(flags TestFlags, cm *ContainerManager) Scope {

	// init from yaml or ini
	spec := NewScopeSpec(*flags.ScopeFile)

	// apply overrides
	if params := flags.Override; params != nil {
		ApplyOverrides(*params, &spec)
		ConfigureSpec(&spec)
	}

	// create provider of resources for scope
	provider := NewProvider(flags, spec.Servers)

	// update defaults from spec based on provider
	for i, _ := range spec.Servers {
		// set default port services
		if spec.Servers[i].RestPort == "" {
			if provider.GetType() == "dev" {
				spec.Servers[i].RestPort = "9000"
			} else {
				spec.Servers[i].RestPort = "8091"
			}
		}

		if spec.Servers[i].ViewPort == "" {
			if provider.GetType() == "dev" {
				spec.Servers[i].ViewPort = "9500"
			} else {
				spec.Servers[i].ViewPort = "8092"
			}
		}
		if spec.Servers[i].QueryPort == "" {
			if provider.GetType() == "dev" {
				// query port for cluster run is based
				// on which node has n1ql service
				// since default behavior is to put n1ql
				// on highest node then this is default port
				spec.Servers[i].QueryPort = fmt.Sprintf("%d", 9500-int(spec.Servers[i].Count))
			} else {
				spec.Servers[i].QueryPort = "8093"
			}
		}
	}
	var loops = 0
	if *flags.Continue == true {
		loops++ // we've already done first pass
	}

	return Scope{
		spec,
		cm,
		provider,
		flags,
		"",
		cmap.New(),
		loops,
	}
}

func (s *Scope) Setup() {

	s.WaitForNodes()
	s.InitCli()
	s.InitNodes()
	s.InitCluster()
	s.AddNodes()
	s.RebalanceClusters()
	s.CreateBuckets()
	s.CreateViews()
}

func (s *Scope) Teardown() {
	// descope
	s.DeleteBuckets()
	s.RemoveNodes()
}

func (s *Scope) InitCli() {

	// make sure proper couchbase-cli is used for node init
	cluster := s.Spec.Servers[0]
	orchestrator := cluster.Names[0]
	rest := s.Provider.GetRestUrl(orchestrator)
	version := GetServerVersion(rest, cluster.RestUsername, cluster.RestPassword)
	s.Version = version[:3]

	// pull cli tag matching version..ie 3.5, 4.1, 4.5
	// :latest is used if no match found
	s.Cm.PullTaggedImage("sequoiatools/couchbase-cli", s.Version)

}

func (s *Scope) WaitForNodes() {

	var image = "martin/wait"

	// use martin/wait container to wait for node to listen on port 8091
	waitForNodesOp := func(name string, server *ServerSpec) {

		ip := s.Provider.GetHostAddress(name)
		ipPort := strings.Split(ip, ":")
		if len(ipPort) == 1 {
			// use default port
			ip = ip + ":8091"
		}

		command := []string{"-c", ip, "-t", "120"}
		desc := "wait for " + ip
		task := ContainerTask{
			Describe: desc,
			Image:    image,
			Command:  command,
			Async:    false,
		}
		if s.Provider.GetType() == "docker" {
			task.LinksTo = name
		}

		s.Cm.Run(&task)
	}

	// verify nodes
	s.Spec.ApplyToAllServers(waitForNodesOp)

}

func (s *Scope) InitNodes() {

	var image = "sequoiatools/couchbase-cli"

	initNodesOp := func(name string, server *ServerSpec) {
		ip := s.Provider.GetHostAddress(name)
		command := []string{"node-init",
			"-c", ip,
			"-u", server.RestUsername,
			"-p", server.RestPassword,
		}

		if s.Provider.GetType() == "file" {
			command = append(command, "--node-init-data-path", server.DataPath)
		}
		if s.Provider.GetType() == "file" {
			command = append(command, "--node-init-index-path", server.IndexPath)
		}
		desc := "init node " + ip
		task := ContainerTask{
			Describe: desc,
			Image:    image,
			Command:  command,
			Async:    false,
		}
		if s.Provider.GetType() == "docker" {
			task.LinksTo = name
		}

		s.Cm.Run(&task)
	}

	// verify nodes
	s.Spec.ApplyToAllServers(initNodesOp)
}

func (s *Scope) InitCluster() {

	var image = "sequoiatools/couchbase-cli"

	initClusterOp := func(name string, server *ServerSpec) {
		orchestrator := server.Names[0]
		ip := s.Provider.GetHostAddress(orchestrator)
		servicesList := server.NodeServices[name]
		services := strings.Join(servicesList, ",")
		ramQuota := server.Ram
		if ramQuota == "" {
			// use cluster mcdReserved
			memTotal := s.ClusterMemReserved(name, server)
			ramQuota = strconv.Itoa(memTotal)
		}
		if strings.Index(ramQuota, "%") > -1 {
			// use percentage of memtotal
			ramQuota = s.GetPercOfMemTotal(name, server, ramQuota)
		}

		// update ramQuota in case modified
		server.Ram = ramQuota
		command := []string{"cluster-init",
			"-c", ip,
			"-u", server.RestUsername,
			"-p", server.RestPassword,
			"--cluster-username", server.RestUsername,
			"--cluster-password", server.RestPassword,
			"--cluster-port", server.RestPort,
			"--cluster-ramsize", server.Ram,
			"--services", services,
		}

		// make sure if index services is specified that index ram is set
		if strings.Index(services, "index") > -1 && server.IndexRam == "" {
			server.IndexRam = strconv.Itoa(s.ClusterIndexQuota(name, server))
		}
		if server.IndexRam != "" {
			indexQuota := server.IndexRam
			if strings.Index(indexQuota, "%") > -1 {
				// use percentage of memtotal
				indexQuota := s.GetPercOfMemTotal(name, server, indexQuota)
				server.IndexRam = indexQuota
			}
			command = append(command, "--cluster-index-ramsize", server.IndexRam)
		}
		// make sure if fts services is specified that fts ram is set
		if strings.Index(services, "fts") > -1 && server.FtsRam == "" {
			server.FtsRam = "256"
		}
		if server.FtsRam != "" {
			ftsQuota := server.FtsRam
			if strings.Index(ftsQuota, "%") > -1 {
				// use percentage of memtotal
				ftsQuota := s.GetPercOfMemTotal(name, server, ftsQuota)
				server.FtsRam = ftsQuota
			}
			command = append(command, "--cluster-fts-ramsize", server.FtsRam)
		}

		if server.IndexStorage != "" {
			command = append(command, "--index-storage-setting", server.IndexStorage)
		}

		command = cliCommandValidator(s.Version, command)

		desc := "init cluster " + orchestrator
		task := ContainerTask{
			Describe: desc,
			Image:    image,
			Command:  command,
			Async:    false,
		}
		if s.Provider.GetType() == "docker" {
			task.LinksTo = orchestrator
		}
		s.Cm.Run(&task)
		server.NodesActive++

	}

	// apply only to orchestrator
	s.Spec.ApplyToServers(initClusterOp, 0, 1)

}

func (s *Scope) AddNodes() {

	var image = "sequoiatools/couchbase-cli"

	addNodesOp := func(name string, server *ServerSpec) {

		if server.InitNodes <= server.NodesActive {
			return
		}
		orchestrator := server.Names[0]
		orchestratorIp := s.Provider.GetHostAddress(orchestrator)
		ip := s.Provider.GetHostAddress(name)

		if name == orchestrator {
			return // not adding self
		}

		servicesList := server.NodeServices[name]
		services := strings.Join(servicesList, ",")
		command := []string{"server-add",
			"-c", orchestratorIp,
			"-u", server.RestUsername,
			"-p", server.RestPassword,
			"--server-add", ip,
			"--server-add-username", server.RestUsername,
			"--server-add-password", server.RestPassword,
			"--services", services,
		}

		desc := "add node " + ip
		command = cliCommandValidator(s.Version, command)

		task := ContainerTask{
			Describe: desc,
			Image:    image,
			Command:  command,
			Async:    false,
		}
		if s.Provider.GetType() == "docker" {
			task.LinksTo = orchestrator
		}

		s.Cm.Run(&task)
		server.NodesActive++

	}

	// add nodes
	s.Spec.ApplyToAllServers(addNodesOp)
}

func (s *Scope) RebalanceClusters() {

	var image = "sequoiatools/couchbase-cli"
	// configure rebalance operation
	operation := func(name string, server *ServerSpec) {

		orchestrator := server.Names[0]
		orchestratorIp := s.Provider.GetHostAddress(orchestrator)

		command := []string{"rebalance",
			"-c", orchestratorIp,
			"-u", server.RestUsername,
			"-p", server.RestPassword,
		}
		desc := "rebalance cluster " + orchestratorIp
		task := ContainerTask{
			Describe: desc,
			Image:    image,
			Command:  command,
			Async:    false,
		}
		if s.Provider.GetType() == "docker" {
			task.LinksTo = orchestrator
		}

		s.Cm.Run(&task)
	}

	// apply only to orchestrator
	s.Spec.ApplyToServers(operation, 0, 1)

}

func (s *Scope) CreateBuckets() {

	var image = "sequoiatools/couchbase-cli"

	// configure rebalance operation
	operation := func(name string, server *ServerSpec) {

		orchestrator := server.Names[0]
		ip := s.Provider.GetHostAddress(orchestrator)

		for _, bucket := range server.BucketSpecs {
			for _, bucketName := range bucket.Names {
				ramQuota := bucket.Ram
				if strings.Index(ramQuota, "%") > -1 {
					// convert bucket ram to value within context of server ram
					ramQuota = strings.Replace(ramQuota, "%", "", 1)
					ramVal, _ := strconv.Atoi(ramQuota)
					nodeRam, _ := strconv.Atoi(server.Ram)
					ramQuota = strconv.Itoa((nodeRam * ramVal) / 100)
				}
				var replica uint8 = 1
				if bucket.Replica != nil {
					replica = *bucket.Replica
				}
				command := []string{"bucket-create", "-c", ip,
					"-u", server.RestUsername, "-p", server.RestPassword,
					"--bucket", bucketName,
					"--bucket-ramsize", ramQuota,
					"--bucket-type", bucket.Type,
					"--bucket-replica", strconv.Itoa(int(replica)),
					"--enable-flush", "1", "--wait",
				}
				if bucket.Sasl != "" {
					command = append(command, "--bucket-password", bucket.Sasl)
				}
				if bucket.Eviction != "" {
					command = append(command, "--bucket-eviction-policy", bucket.Eviction)
				}

				desc := "bucket create " + bucketName
				task := ContainerTask{
					Describe: desc,
					Image:    image,
					Command:  command,
					Async:    false,
				}
				if s.Provider.GetType() == "docker" {
					task.LinksTo = orchestrator
				}

				s.Cm.Run(&task)
			}
		}
	}

	// apply only to orchestrator
	s.Spec.ApplyToServers(operation, 0, 1)

}

func (s *Scope) GetPercOfMemTotal(name string, server *ServerSpec, quota string) string {
	memTotal := s.ClusterMemTotal(name, server)
	ramQuota := strings.Replace(quota, "%", "", 1)
	ramVal, _ := strconv.Atoi(ramQuota)
	ramQuota = strconv.Itoa((memTotal * ramVal) / 100)
	return ramQuota
}

func (s *Scope) ClusterMemTotal(name string, server *ServerSpec) int {
	rest := s.Provider.GetRestUrl(name)
	mem := GetMemTotal(rest, server.RestUsername, server.RestPassword)
	if s.Provider.GetType() == "docker" {
		p := s.Provider.(*DockerProvider)
		if p.Opts.Memory > 0 {
			mem = p.Opts.MemoryMB()
		}
	}
	return mem
}

func (s *Scope) ClusterMemReserved(name string, server *ServerSpec) int {
	rest := s.Provider.GetRestUrl(name)
	mem := GetMemReserved(rest, server.RestUsername, server.RestPassword)
	return mem
}

func (s *Scope) ClusterIndexQuota(name string, server *ServerSpec) int {
	rest := s.Provider.GetRestUrl(name)
	mem := GetIndexQuota(rest, server.RestUsername, server.RestPassword)
	return mem
}

func (s *Scope) CompileCommand(actionCommand string) []string {

	// remove extraneous white space
	re := regexp.MustCompile(`\s+`)
	actionCommand = re.ReplaceAllString(actionCommand, " ")

	// parse template
	actionCommand = ParseTemplate(s, actionCommand)

	// translate into in slice
	command := strings.Split(actionCommand, " ")
	commandFinal := []string{}

	// keep single quotes in tact
	var lastSingleQuote int = -1
	for i, v := range command {
		if strings.Index(v, "'") > -1 {
			// stash val until we reach another single quote
			if lastSingleQuote == -1 {
				// first quote
				lastSingleQuote = i
			} else {
				// ending quote
				var quotedString = []string{}
				for j := lastSingleQuote; j <= i; j++ {
					c := strings.Replace(command[j], "'", "", 1)
					quotedString = append(quotedString, c)
				}
				// append to command as fully quoted string
				commandFinal = append(commandFinal, strings.Join(quotedString, " "))
				lastSingleQuote = -1
			}
		} else {
			// just append value if not within single quote
			if lastSingleQuote == -1 {
				commandFinal = append(commandFinal, v)
			}
		}
	}

	return commandFinal
}

//
// cliCommandValidator checks the cli command for opts that
// could possibly be invalid based on version
//
func cliCommandValidator(version string, command []string) []string {

	if version == "" {
		fmt.Println("version not set")
		return command
	}

	result := []string{}
	vMajor, _ := strconv.ParseFloat(version, 64)

	for i, arg := range command {
		if i == 0 {
			// action
			result = append(result, command[0])
			continue
		}
		// must be an arg
		if strings.Index(arg, "-") != 0 {
			continue
		}

		// <4.5 builds
		if vMajor < 4.5 {
			if arg == "--index-storage-setting" ||
				arg == "--cluster-fts-ramsize" {
				continue
			}
		}

		// <4.0 builds
		if vMajor < 4.0 && (arg == "--services" ||
			arg == "--cluster-index-ramsize" ||
			arg == "--node-init-index-path") {
			continue
		}

		// copy arg
		result = append(result, arg)
		// check if arg has value
		if i+1 < len(command) {
			result = append(result, command[i+1])
		}
	}

	return result
}

func (s *Scope) CreateViews() {

	var image = "appropriate/curl"

	operation := func(name string, server *ServerSpec) {

		orchestrator := server.Names[0]
		ip := s.Provider.GetHostAddress(orchestrator)

		// for each bucket name
		for _, bucket := range server.BucketSpecs {
			for _, bucketName := range bucket.Names {

				// add ddocs to bucket
				for _, ddoc := range bucket.DDocSpecs {

					// combine ddoc views
					var ddocDef = DDocToJson(ddoc)

					// compose view create command
					ip = strings.Split(ip, ":")[0]
					viewUrl := fmt.Sprintf("http://%s:%s/%s/_design/%s",
						ip, server.ViewPort, bucketName, ddoc.Name)
					command := []string{"-s", "-X", "PUT",
						"-u", server.RestUsername + ":" + server.RestPassword,
						"-H", "Content-Type:application/json",
						viewUrl,
						"-d", ddocDef,
					}
					desc := "views create" + bucketName
					task := ContainerTask{
						Describe: desc,
						Image:    image,
						Command:  command,
						Async:    false,
					}
					if s.Provider.GetType() == "docker" {
						task.LinksTo = orchestrator
					}
					s.Cm.Run(&task)
				}
			}
		}
	}

	// apply only to orchestrator
	s.Spec.ApplyToServers(operation, 0, 1)

}

func (s *Scope) DeleteBuckets() {

	var image = "sequoiatools/couchbase-cli"

	// configure rebalance operation
	operation := func(name string, server *ServerSpec) {

		orchestrator := server.Names[0]
		ip := s.Provider.GetHostAddress(orchestrator)

		for _, bucket := range server.BucketSpecs {
			for _, bucketName := range bucket.Names {

				command := []string{"bucket-delete", "-c", ip,
					"-u", server.RestUsername, "-p", server.RestPassword,
					"--bucket", bucketName,
				}

				desc := "bucket delete" + bucketName
				task := ContainerTask{
					Describe: desc,
					Image:    image,
					Command:  command,
					Async:    false,
				}
				if s.Provider.GetType() == "docker" {
					task.LinksTo = orchestrator
				}

				s.Cm.Run(&task)
			}
		}
	}

	// apply only to orchestrator
	s.Spec.ApplyToServers(operation, 0, 1)

}

func (s *Scope) RemoveNodes() {

	var image = "sequoiatools/couchbase-cli"

	rmNodesOp := func(name string, server *ServerSpec) {

		orchestrator := server.Names[0]
		orchestratorIp := s.Provider.GetHostAddress(orchestrator)
		ip := s.Provider.GetHostAddress(name)

		if name == orchestrator {
			return // not removing self
		}

		command := []string{"rebalance",
			"-c", orchestratorIp,
			"-u", server.RestUsername,
			"-p", server.RestPassword,
			"--server-remove", ip,
		}

		desc := "remove node " + ip
		command = cliCommandValidator(s.Version, command)

		task := ContainerTask{
			Describe: desc,
			Image:    image,
			Command:  command,
			Async:    false,
		}
		if s.Provider.GetType() == "docker" {
			task.LinksTo = orchestrator
		}

		s.Cm.Run(&task)
	}

	// add nodes
	s.Spec.ApplyToAllServers(rmNodesOp)
}

func (s *Scope) GetPlatform() string {
	return *s.Flags.Platform
}

func (s *Scope) SetVarsKV(key, id string) {
	s.Vars.Set(key, id)
}

func (s *Scope) GetVarsKV(key string) (string, bool) {
	if val, ok := s.Vars.Get(key); ok {
		return val.(string), ok
	} else {
		return "", false
	}
}
