// estate_positions extracts absolute latitude/longitude (in degrees) for
// every EstimatedState message in an LSF log.
//
// The IMC EstimatedState carries a geodetic reference (Lat, Lon in radians,
// Height in meters) together with a NED offset (X = north, Y = east,
// Z = down, in meters). The absolute position is obtained by displacing the
// reference by the NED offset using the WGS84 ellipsoid.
//
// Usage: estate_positions <IMC.xml> <Data.lsf>
package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"strings"

	"github.com/oceanscan/imc-go"
)

type gzReadCloser struct {
	*gzip.Reader
	f *os.File
}

func (g *gzReadCloser) Close() error {
	err := g.Reader.Close()
	if cerr := g.f.Close(); err == nil {
		err = cerr
	}
	return err
}

func openMaybeGz(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(path, ".gz") {
		return f, nil
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &gzReadCloser{Reader: gz, f: f}, nil
}

const (
	wgs84A = 6378137.0           // semi-major axis [m]
	wgs84F = 1.0 / 298.257223563 // flattening
)

var wgs84E2 = wgs84F * (2 - wgs84F) // first eccentricity squared

// displaceWGS84 returns absolute (lat, lon) in radians given a reference
// (latRef, lonRef) in radians and a NED offset (n, e) in meters.
func displaceWGS84(latRef, lonRef, n, e float64) (float64, float64) {
	sinLat := math.Sin(latRef)
	tmp := 1 - wgs84E2*sinLat*sinLat
	// meridian radius of curvature
	rm := wgs84A * (1 - wgs84E2) / math.Pow(tmp, 1.5)
	// prime vertical radius of curvature
	rn := wgs84A / math.Sqrt(tmp)

	lat := latRef + n/rm
	lon := lonRef + e/(rn*math.Cos(latRef))
	return lat, lon
}

func main() {
	if len(os.Args) != 3 {
		log.Fatalf("usage: %s <IMC.xml[.gz]> <Data.lsf[.gz]>", os.Args[0])
	}
	xmlPath, lsfPath := os.Args[1], os.Args[2]

	xr, err := openMaybeGz(xmlPath)
	if err != nil {
		log.Fatalf("open IMC.xml: %v", err)
	}
	xmlProto, err := imc.ParseReader(xr)
	xr.Close()
	if err != nil {
		log.Fatalf("parse IMC.xml: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	f, err := openMaybeGz(lsfPath)
	if err != nil {
		log.Fatalf("open lsf: %v", err)
	}
	defer f.Close()

	fmt.Println("timestamp,src,src_ent,lat_deg,lon_deg,height_m,depth_m")

	for {
		msg, err := proto.UnmarshalReader(f)
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatalf("unmarshal: %v", err)
		}

		if proto.Messages[msg.Header.MGID].Abbrev != "EstimatedState" {
			continue
		}

		es := &imc.EstimatedState{}
		if err := es.FromMessage(msg); err != nil {
			log.Printf("decode EstimatedState: %v", err)
			continue
		}

		lat, lon := displaceWGS84(es.Lat, es.Lon, float64(es.X), float64(es.Y))
		latDeg := lat * 180.0 / math.Pi
		lonDeg := lon * 180.0 / math.Pi

		fmt.Printf("%.6f,%d,%d,%.8f,%.8f,%.3f,%.3f\n",
			es.Header.Timestamp, es.Header.Src, es.Header.SrcEnt,
			latDeg, lonDeg, es.Height-float32(es.Z), es.Depth)
	}
}
