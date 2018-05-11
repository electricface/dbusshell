package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"os/exec"

	"github.com/abiosoft/ishell"
	ofdbus "github.com/linuxdeepin/go-dbus-factory/org.freedesktop.dbus"
	"pkg.deepin.io/lib/dbus1"
	"pkg.deepin.io/lib/dbus1/introspect"
)

var conn *dbus.Conn
var dbusName string
var dbusPath = "/"
var dbusOldPath = "/"
var dbusInterface string
var logger *log.Logger
var logFile string

var dbusType int

const (
	dbusTypeNil = iota
	dbusTypeSession
	dbusTypeSystem
	dbusTypeOther
)

func init() {
	logFile = fmt.Sprintf("/tmp/dbusshell-%d", os.Getpid())
	f, err := os.Create(logFile)
	if err != nil {
		panic(err)
	}

	logger = log.New(f, "", log.Lshortfile)
}

func fixArgsName(args []introspect.Arg) {
	if len(args) > 0 {
		if args[0].Name == "" {
			for idx := range args {
				args[idx].Name = "arg" + strconv.Itoa(idx)
			}
		}
	}
}

func getMethodArgsString(args []introspect.Arg) string {
	var inArgs []string
	var outArgs []string

	fixArgsName(args)
	for _, arg := range args {
		if arg.Direction == "in" {
			inArgs = append(inArgs, arg.Name+" "+arg.Type)
		} else if arg.Direction == "out" {
			outArgs = append(outArgs, arg.Name+" "+arg.Type)
		}
	}
	return fmt.Sprintf("(%s) -> (%s)",
		strings.Join(inArgs, ", "),
		strings.Join(outArgs, ", "))
}

func getSignalArgsString(args []introspect.Arg) string {
	var argStrv []string
	fixArgsName(args)
	for _, arg := range args {
		argStrv = append(argStrv, arg.Name+" "+arg.Type)
	}
	return fmt.Sprintf("(%s)", strings.Join(argStrv, ", "))
}

func showInterface(ctx *ishell.Context, ifc introspect.Interface) {
	var buf bytes.Buffer
	buf.WriteString("interface: " + ifc.Name + "\n")

	if len(ifc.Methods) > 0 {
		buf.WriteString(" methods:\n")
		for _, method := range ifc.Methods {
			argsStr := getMethodArgsString(method.Args)
			buf.WriteString("  " + method.Name + argsStr + "\n")
		}
	}

	if len(ifc.Properties) > 0 {
		buf.WriteString(" Properties:\n")
		for _, prop := range ifc.Properties {
			buf.WriteString(fmt.Sprintf("  %s %s %s\n", prop.Name, prop.Type, prop.Access))
		}
	}

	if len(ifc.Signals) > 0 {
		buf.WriteString(" Signals:\n")
		for _, signal := range ifc.Signals {
			argsStr := getSignalArgsString(signal.Args)
			buf.WriteString("  " + signal.Name + argsStr + "\n")
		}
	}
	ctx.Printf("%s", buf.Bytes())
}

func main() {
	shell := ishell.New()
	shell.Println("dbus shell")

	shell.AddCmd(&ishell.Cmd{
		Name: "conn",
		Func: func(c *ishell.Context) {
			if len(c.Args) == 1 {
				arg0 := c.Args[0]
				if arg0 == "session" || arg0 == "e" {
					var err error
					conn, err = dbus.SessionBus()
					if err != nil {
						c.Err(err)
						return
					}
					dbusType = dbusTypeSession
				} else if arg0 == "system" || arg0 == "y" {
					var err error
					conn, err = dbus.SystemBus()
					if err != nil {
						c.Err(err)
						return
					}
					dbusType = dbusTypeSystem
				}
			}

			if conn != nil {
				c.Println(getConnDesc())
				c.Printf("names: %v\n", conn.Names())
			} else {
				c.Println("conn is nil")
			}
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "ls-services",
		Func: func(c *ishell.Context) {
			if conn == nil {
				c.Println("conn is nil")
				return
			}

			ofd := ofdbus.NewDBus(conn)
			names, err := ofd.ListNames(0)
			if err != nil {
				c.Err(err)
				return
			}

			var buf bytes.Buffer
			for _, name := range names {
				if !strings.HasPrefix(name, ":") {
					buf.WriteString(name)
					buf.WriteByte('\n')
				}
			}
			c.ShowPaged(buf.String())
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name:    "change-service",
		Aliases: []string{"cs"},
		Func: func(c *ishell.Context) {
			if len(c.Args) == 1 {
				hasOwner, err := getNameHasOwner(c.Args[0])
				if err != nil {
					c.Err(err)
					return
				}
				if !hasOwner {
					c.Println("name has not owner")
					return
				}

				dbusName = c.Args[0]
				dbusPath = "/"
			}
			c.Printf("dbusName: %q\n", dbusName)
		},
		Completer: func(args []string) []string {
			return getServices()
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "ls",
		Func: func(c *ishell.Context) {
			node, err := getNode()
			if err != nil {
				c.Err(err)
				return
			}

			for _, ifc := range node.Interfaces {
				ifcName := ifc.Name
				if ifc.Name == dbusInterface {
					ifcName += "*"
				}
				c.Println(ifcName)
			}
			for _, child := range node.Children {
				c.Println(child.Name + "/")
			}
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "show",
		Func: func(c *ishell.Context) {
			if dbusInterface == "" {
				return
			}

			node, err := getNode()
			if err != nil {
				c.Err(err)
				return
			}

			for _, ifc := range node.Interfaces {
				if ifc.Name == dbusInterface {
					showInterface(c, ifc)
				}
			}
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "cd",
		Func: func(c *ishell.Context) {
			var newPath string
			if len(c.Args) == 1 && c.Args[0] == "-" {
				newPath = dbusOldPath
			} else if len(c.Args) == 1 && c.Args[0] == "$" {
				// service to path
				newPath = "/" + strings.Replace(dbusName, ".", "/", -1)
			} else {
				newPath = getNewPath(c.Args)
			}

			if !dbus.ObjectPath(newPath).IsValid() {
				c.Println("path is invalid")
				return
			}

			node, err := getNodeWithPath(newPath)
			if err != nil {
				c.Err(err)
				return
			}

			if len(node.Interfaces)+len(node.Children) == 0 {
				c.Println("failed to cd to", newPath)
				return
			}

			c.Println("cd to", newPath)
			dbusOldPath = dbusPath
			dbusPath = newPath

			// auto select interface
			for _, ifc := range node.Interfaces {
				if dbusInterface == ifc.Name {
					return
				}
				if !isStandardDBusInterface(ifc.Name) {
					dbusInterface = ifc.Name
					c.Println("auto select interface:", dbusInterface)
					return
				}
			}
			dbusInterface = ""
		},
		Completer: func(args []string) (result []string) {
			logger.Println("cd completer args:", args)

			newPath := getNewPath(args)
			logger.Println("newPath:", newPath)
			node, err := getNodeWithPath(newPath)
			if err != nil {
				return nil
			}

			for _, child := range node.Children {
				result = append(result, child.Name)
			}
			return
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "get",
		Func: func(c *ishell.Context) {
			if len(c.Args) != 1 {
				return
			}

			if conn == nil {
				c.Println("conn is nil")
				return
			}

			arg0 := c.Args[0]
			obj := conn.Object(dbusName, dbus.ObjectPath(dbusPath))
			var variant dbus.Variant
			err := obj.Call("org.freedesktop.DBus.Properties.Get", 0,
				dbusInterface, arg0).Store(&variant)
			if err != nil {
				c.Err(err)
				return
			}
			c.Println(formatVariant(variant))
		},
		Completer: func(args []string) (result []string) {
			ifc, err := getInterface()
			if err != nil {
				return nil
			}

			for _, prop := range ifc.Properties {
				result = append(result, prop.Name)
			}
			return
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "get-all",
		Func: func(c *ishell.Context) {
			if conn == nil {
				c.Println("conn is nil")
				return
			}

			obj := conn.Object(dbusName, dbus.ObjectPath(dbusPath))
			var props map[string]dbus.Variant
			err := obj.Call("org.freedesktop.DBus.Properties.GetAll", 0,
				dbusInterface).Store(&props)
			if err != nil {
				c.Err(err)
				return
			}
			var propNames []string
			for propName := range props {
				propNames = append(propNames, propName)
			}
			sort.Strings(propNames)

			for _, propName := range propNames {
				propValue := props[propName]
				c.Print(propName + ": ")
				c.Println(formatVariant(propValue))
			}
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "pwd",
		Func: func(c *ishell.Context) {
			c.Println(dbusPath)
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "info",
		Func: func(c *ishell.Context) {
			c.Println(getConnDesc())
			c.Println("service:", dbusName)
			c.Println("path:", dbusPath)
			c.Println("interface:", dbusInterface)
			c.Println("pid:", os.Getpid())
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name:    "interface",
		Aliases: []string{"ifc"},
		Func: func(c *ishell.Context) {
			if len(c.Args) == 1 {
				arg0 := c.Args[0]

				// check interface
				var ifcOk bool
				for _, ifc := range getInterfaces() {
					if ifc == arg0 {
						ifcOk = true
						break
					}
				}
				if !ifcOk {
					c.Println("invalid interface")
					return
				}

				dbusInterface = arg0
				c.Println("select interface:", arg0)
			}
		},
		Completer: func(args []string) []string {
			return getInterfaces()
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "call",
		Func: func(c *ishell.Context) {
			c.Printf("call args: %#v\n", c.Args)
			connTypeOpt, err := getGDBusConnTypeOpt()
			if err != nil {
				c.Err(err)
				return
			}

			var args = []string{
				"call", connTypeOpt,
				"-d", dbusName,
				"-o", dbusPath,
				"-m", dbusInterface + "." + c.Args[0],
			}
			args = append(args, c.Args[1:]...)
			cmd := exec.Command("gdbus", args...)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Run()
			if err != nil {
				c.Err(err)
			}
			return
		},
		Completer: func(args []string) (result []string) {
			ifc, err := getInterface()
			if err != nil {
				return nil
			}

			for _, method := range ifc.Methods {
				result = append(result, method.Name)
			}

			return
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "tmux-buffer",
		Func: func(c *ishell.Context) {
			cmd := exec.Command("tmux", "show-buffer")
			out, err := cmd.Output()
			if err != nil {
				c.Err(err)
			}
			if json.Valid(out) {
				var buf bytes.Buffer
				err = json.Indent(&buf, out, "", "  ")
				if err != nil {
					c.Err(err)
					return
				}
				c.Printf("%s\n", buf.Bytes())
			} else {
				c.Printf("%s\n", out)
			}
		},
	})

	shell.AddCmd(&ishell.Cmd{
		Name: "help-types",
		Func: func(c *ishell.Context) {
			c.Print(`Base Types:
bool b
byte y
int16 n uint16 q
int32 i uint32 u
int64 x uint64 t
double d
unixFd h

as -> []string
a{ss} -> map[string]string
(is) struct{int32,string}
`)
		},
	})

	shell.Run()
	shell.Close()

	// remove log file
	err := os.Remove(logFile)
	if err != nil {
		log.Printf("warning: failed to remove logFile %q: %v\n", logFile, err)
	}
}

func getConnDesc() string {
	switch dbusType {
	case dbusTypeNil:
		return "conn is nil"
	case dbusTypeSession:
		return "session bus"
	case dbusTypeSystem:
		return "system bus"
	case dbusTypeOther:
		return "other bus"
	default:
		return "unknown bus"
	}
}

func getGDBusConnTypeOpt() (string, error) {
	switch dbusType {
	case dbusTypeNil:
		return "", errors.New("conn is nil")
	case dbusTypeSession:
		return "-e", nil
	case dbusTypeSystem:
		return "-y", nil
	case dbusTypeOther:
		return "", errors.New("not supported")
	default:
		return "", errors.New("unknown dbus type")
	}
}

func isStandardDBusInterface(ifcName string) bool {
	switch ifcName {
	case "org.freedesktop.DBus.Introspectable",
		"org.freedesktop.DBus.Properties",
		"org.freedesktop.DBus.Peer":
		return true
	default:
		return false
	}
}

func getNewPath(args []string) string {
	var newPath string

	if len(args) == 1 && strings.HasPrefix(args[0], "/") {
		newPath = args[0]
	} else {
		joinArgs := append([]string{dbusPath}, args...)
		newPath = filepath.Join(joinArgs...)
	}

	newPath = filepath.Clean(newPath)
	return newPath
}

func formatVariant(variant dbus.Variant) string {
	val := variant.Value()
	switch v := val.(type) {
	case string:
		// is json?
		data := []byte(v)
		if json.Valid(data) {
			var buf bytes.Buffer
			json.Indent(&buf, data, "", "  ")
			return fmt.Sprintf("%s", buf.Bytes())
		} else {
			return v
		}
	default:
		return fmt.Sprintf("%#v", val)
	}
}

func getInterface() (*introspect.Interface, error) {
	node, err := getNode()
	if err != nil {
		return nil, err
	}

	if dbusInterface == "" {
		return nil, errors.New("dbusInterface is empty")
	}

	for _, ifc := range node.Interfaces {
		if ifc.Name == dbusInterface {
			return &ifc, nil
		}
	}
	return nil, errors.New("not found interface")
}

func validDBusName(name string) bool {
	if name == "" {
		return false
	}
	parts := strings.Split(name, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
	}
	return true
}

func getNodeWithPath(path string) (*introspect.Node, error) {
	if conn == nil {
		return nil, errors.New("conn is nil")
	}

	if !validDBusName(dbusName) {
		return nil, errors.New("dbusName is invalid")
	}

	obj := conn.Object(dbusName, dbus.ObjectPath(path))
	var xmlStr string
	err := obj.Call("org.freedesktop.DBus.Introspectable.Introspect",
		0).Store(&xmlStr)
	if err != nil {
		return nil, err
	}
	var node introspect.Node
	err = xml.Unmarshal([]byte(xmlStr), &node)
	if err != nil {
		return nil, err
	}
	return &node, nil
}

func getNode() (*introspect.Node, error) {
	return getNodeWithPath(dbusPath)
}

func getInterfaces() (result []string) {
	node, err := getNode()
	if err != nil {
		return nil
	}

	for _, ifc := range node.Interfaces {
		result = append(result, ifc.Name)
	}
	return
}

func getServices() (result []string) {
	if conn == nil {
		return nil
	}

	ofd := ofdbus.NewDBus(conn)
	names, err := ofd.ListNames(0)
	if err != nil {
		return
	}

	for _, name := range names {
		if !strings.HasPrefix(name, ":") {
			result = append(result, name)
		}
	}
	return
}

func getNameHasOwner(name string) (bool, error) {
	if conn == nil {
		return false, errors.New("conn is nil")
	}
	ofd := ofdbus.NewDBus(conn)
	return ofd.NameHasOwner(0, name)
}
