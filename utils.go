package main

import (
	"fmt"
	"net"
	"reflect"
)

// findIPAddresses scans the network interfaces of the local machine and returns a slice of IP addresses (as strings) that are active and not loopback addresses.
func findIPAddresses() ([]string, error) {
	var ips []string

	// Get a list of all interfaces.
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	// Iterate through each interface.
	for _, iface := range ifaces {
		// Check if the interface is up and is not a loopback interface.
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue // Interface is down or is a loopback; skip it.
		}

		// Get the addresses associated with the interface.
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, err
		}

		// Process the addresses.
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			// Make sure it's an IPv4 address and not a loopback address.
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip != nil { // If it's a valid IPv4 address
				ips = append(ips, ip.String())
			}
		}
	}

	return ips, nil
}

func printFields(v reflect.Value, indent string) {
	// Ensure the value is valid
	if !v.IsValid() {
		fmt.Println(indent + "Invalid Value")
		return
	}

	// Dereference pointers to get the value they point to
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			fmt.Println(indent + "nil")
			return
		}
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Struct:
		t := v.Type()
		fmt.Println(indent + t.Name() + " {")
		for i := 0; i < v.NumField(); i++ {
			// Skip unexported fields
			if !v.Field(i).CanInterface() {
				fmt.Printf("%s  %s: (unexported field)\n", indent, t.Field(i).Name)
				continue
			}
			fmt.Printf("%s  %s: ", indent, t.Field(i).Name)
			printFields(v.Field(i), indent+"  ")
		}
		fmt.Println(indent + "}")
	case reflect.Slice, reflect.Array:
		fmt.Println(indent + "[")
		for i := 0; i < v.Len(); i++ {
			printFields(v.Index(i), indent+"  ")
		}
		fmt.Println(indent + "]")
	case reflect.Interface:
		// Extract the element the interface points to
		actualValue := v.Elem()
		if !actualValue.IsValid() {
			fmt.Println(indent + "nil")
		} else {
			printFields(actualValue, indent)
		}
	default:
		// Print the value
		fmt.Println(indent + fmt.Sprintf("%v", v.Interface()))
	}
}

// PrintAllFields is a wrapper that starts the recursive printing
func PrintAllFields(obj interface{}) {
	fmt.Println("--------------------------------")
	fmt.Println("Start printing fields of", reflect.TypeOf(obj))
	v := reflect.ValueOf(obj)
	printFields(v, "")
}
