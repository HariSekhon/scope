package endpoint

import (
	"log"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/weaveworks/procspy"
	"github.com/weaveworks/scope/probe/process"
	"github.com/weaveworks/scope/report"
)

// Node metadata keys.
const (
	Addr = "addr" // typically IPv4
	Port = "port"
)

// Reporter generates Reports containing the Endpoint topology.
type Reporter struct {
	hostID           string
	hostName         string
	includeProcesses bool
	includeNAT       bool
	conntracker      *Conntracker
	natmapper        *natmapper
}

// SpyDuration is an exported prometheus metric
var SpyDuration = prometheus.NewSummaryVec(
	prometheus.SummaryOpts{
		Namespace: "scope",
		Subsystem: "probe",
		Name:      "spy_time_nanoseconds",
		Help:      "Total time spent spying on active connections.",
		MaxAge:    10 * time.Second, // like statsd
	},
	[]string{},
)

// NewReporter creates a new Reporter that invokes procspy.Connections to
// generate a report.Report that contains every discovered (spied) connection
// on the host machine, at the granularity of host and port. That information
// is stored in the Endpoint topology. It optionally enriches that topology
// with process (PID) information.
func NewReporter(hostID, hostName string, includeProcesses bool, useConntrack bool) *Reporter {
	var (
		conntrackModulePresent = ConntrackModulePresent()
		conntracker            *Conntracker
		natmapper              *natmapper
		err                    error
	)
	if conntrackModulePresent && useConntrack {
		conntracker, err = NewConntracker()
		if err != nil {
			log.Printf("Failed to start conntracker: %v", err)
		}
	}
	if conntrackModulePresent {
		natmapper, err = newNATMapper()
		if err != nil {
			log.Printf("Failed to start natMapper: %v", err)
		}
	}
	return &Reporter{
		hostID:           hostID,
		hostName:         hostName,
		includeProcesses: includeProcesses,
		conntracker:      conntracker,
		natmapper:        natmapper,
	}
}

// Stop stop stop
func (r *Reporter) Stop() {
	if r.conntracker != nil {
		r.conntracker.Stop()
	}
	if r.natmapper != nil {
		r.natmapper.Stop()
	}
}

// Report implements Reporter.
func (r *Reporter) Report() (report.Report, error) {
	defer func(begin time.Time) {
		SpyDuration.WithLabelValues().Observe(float64(time.Since(begin)))
	}(time.Now())

	rpt := report.MakeReport()
	conns, err := procspy.Connections(r.includeProcesses)
	if err != nil {
		return rpt, err
	}

	for conn := conns.Next(); conn != nil; conn = conns.Next() {
		var (
			localPort  = conn.LocalPort
			remotePort = conn.RemotePort
			localAddr  = conn.LocalAddress.String()
			remoteAddr = conn.RemoteAddress.String()
		)
		r.addConnection(&rpt, localAddr, remoteAddr, localPort, remotePort, &conn.Proc)
	}

	if r.conntracker != nil {
		r.conntracker.WalkFlows(func(f Flow) {
			var (
				localPort  = f.Original.Layer4.SrcPort
				remotePort = f.Original.Layer4.DstPort
				localAddr  = f.Original.Layer3.SrcIP
				remoteAddr = f.Original.Layer3.DstIP
			)
			r.addConnection(&rpt, localAddr, remoteAddr, uint16(localPort), uint16(remotePort), nil)
		})
	}

	if r.natmapper != nil {
		r.natmapper.applyNAT(rpt, r.hostID)
	}

	return rpt, err
}

func (r *Reporter) addConnection(rpt *report.Report, localAddr, remoteAddr string, localPort, remotePort uint16, proc *procspy.Proc) {
	localIsClient := int(localPort) > int(remotePort)

	// Update address topology
	{
		var (
			localAddressNodeID  = report.MakeAddressNodeID(r.hostID, localAddr)
			remoteAddressNodeID = report.MakeAddressNodeID(r.hostID, remoteAddr)
			adjacencyID         = ""
			edgeID              = ""
		)

		if localIsClient {
			adjacencyID = report.MakeAdjacencyID(localAddressNodeID)
			rpt.Address.Adjacency[adjacencyID] = rpt.Address.Adjacency[adjacencyID].Add(remoteAddressNodeID)

			edgeID = report.MakeEdgeID(localAddressNodeID, remoteAddressNodeID)
		} else {
			adjacencyID = report.MakeAdjacencyID(remoteAddressNodeID)
			rpt.Address.Adjacency[adjacencyID] = rpt.Address.Adjacency[adjacencyID].Add(localAddressNodeID)

			edgeID = report.MakeEdgeID(remoteAddressNodeID, localAddressNodeID)
		}

		countTCPConnection(rpt.Address.EdgeMetadatas, edgeID)

		if _, ok := rpt.Address.NodeMetadatas[localAddressNodeID]; !ok {
			rpt.Address.NodeMetadatas[localAddressNodeID] = report.MakeNodeMetadataWith(map[string]string{
				"name": r.hostName,
				Addr:   localAddr,
			})
		}
	}

	// Update endpoint topology
	if r.includeProcesses {
		var (
			localEndpointNodeID  = report.MakeEndpointNodeID(r.hostID, localAddr, strconv.Itoa(int(localPort)))
			remoteEndpointNodeID = report.MakeEndpointNodeID(r.hostID, remoteAddr, strconv.Itoa(int(remotePort)))
			adjacencyID          = ""
			edgeID               = ""
		)

		if localIsClient {
			adjacencyID = report.MakeAdjacencyID(localEndpointNodeID)
			rpt.Endpoint.Adjacency[adjacencyID] = rpt.Endpoint.Adjacency[adjacencyID].Add(remoteEndpointNodeID)

			edgeID = report.MakeEdgeID(localEndpointNodeID, remoteEndpointNodeID)
		} else {
			adjacencyID = report.MakeAdjacencyID(remoteEndpointNodeID)
			rpt.Endpoint.Adjacency[adjacencyID] = rpt.Endpoint.Adjacency[adjacencyID].Add(localEndpointNodeID)

			edgeID = report.MakeEdgeID(remoteEndpointNodeID, localEndpointNodeID)
		}

		countTCPConnection(rpt.Endpoint.EdgeMetadatas, edgeID)

		md, ok := rpt.Endpoint.NodeMetadatas[localEndpointNodeID]
		updated := !ok
		if !ok {
			md = report.MakeNodeMetadataWith(map[string]string{
				Addr: localAddr,
				Port: strconv.Itoa(int(localPort)),
			})
		}
		if proc != nil && proc.PID > 0 {
			pid := strconv.FormatUint(uint64(proc.PID), 10)
			updated = updated || md.Metadata[process.PID] != pid
			md.Metadata[process.PID] = pid
		}
		if updated {
			rpt.Endpoint.NodeMetadatas[localEndpointNodeID] = md
		}
	}
}

func countTCPConnection(mds report.EdgeMetadatas, key string) {
	md := mds[key]
	if md.MaxConnCountTCP == nil {
		md.MaxConnCountTCP = new(uint64)
	}
	*md.MaxConnCountTCP++
	mds[key] = md
}
