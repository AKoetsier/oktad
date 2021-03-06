package main

import "fmt"

import "time"
import "errors"
import "strconv"
import "net/http"
import "github.com/jessevdk/go-flags"
import "github.com/tj/go-debug"
import "github.com/peterh/liner"
import "github.com/aws/aws-sdk-go/aws/credentials"
import "github.com/havoc-io/go-keytar"

const VERSION = "0.7.0"
const SESSION_COOKIE = "__oktad_session_cookie"

func main() {
	var opts struct {
		ConfigFile          string `short:"c" long:"config" description:"Path to config file" default:"~/.oktad/config"`
		PrintVersion        bool   `short:"v" long:"version" description:"Print version number and exit"`
		ForceNewCredentials bool   `short:"f" long:"force-new" description:"force new credentials"`
		ProfileName         string `short:"p" long:"profile-name" description:"Profile name to save the credentials to"`
	}

	debug := debug.Debug("oktad:main")
	args, err := flags.Parse(&opts)

	if err != nil {
		return
	}

	if opts.PrintVersion {
		fmt.Printf("oktad v%s\n", VERSION)
		return
	}

	debug("loading configuration data")
	// try to load configuration
	oktaCfg, err := parseConfig(opts.ConfigFile)

	if err != nil {
		fmt.Println("Error reading config file!")
		debug("cfg read err: %s", err)
		return
	}

	if len(args) <= 0 {
		fmt.Println("Hey, that command won't actually do anything.\n\nSorry.")
		return
	}

	awsProfile := args[0]
	acfg, err := readAwsProfile(
		fmt.Sprintf("profile %s", awsProfile),
	)

	var skipSecondRole bool

	if err != nil {
		//fmt.Println("Error reading your AWS profile!")
		debug("error reading AWS profile: %s", err)
		if err == awsProfileNotFound {
			// if the AWS profile isn't found, we'll assume that
			// the user intends to run a command in the first account
			// behind their okta auth, rather than assuming role twice
			skipSecondRole = true
			fmt.Printf(
				"We couldn't find an AWS profile named %s,\nso we will AssumeRole into your base account.\n",
				awsProfile,
			)
			awsProfile = BASE_PROFILE_CREDS

			args = append([]string{BASE_PROFILE_CREDS}, args...)
		}
	}

	if !opts.ForceNewCredentials {
		maybeCreds, err := loadCreds(awsProfile)
		if err == nil {
			debug("found cached credentials, going to use them")
			// if we could load creds, use them!
			err := prepAndLaunch(args, maybeCreds)
			if err != nil {
				fmt.Println("Error launching program: ", err)
			}
			return
		}

		debug("cred load err %s", err)
	}

	keystore, err := keytar.GetKeychain()
	if err != nil {
		fmt.Println("Failed to get keychain access")
		debug("error was %s", err)
		return
	}

	var sessionToken string
	var saml *OktaSamlResponse
	password, err := keystore.GetPassword(APPNAME, SESSION_COOKIE)
	if err != nil || password == "" {
		sessionToken, err = getSessionFromLogin(&oktaCfg)
		if err != nil {
			return
		}

		saml, err = getSaml(&oktaCfg, sessionToken)
		if err != nil {
			fmt.Println("Error parsing SAML response")
			debug("error was %s", err)
			return
		}
	}

	if saml == nil || saml.raw == "" {
		// We got a saved session

		cookie := http.Cookie{}
		err = decodePasswordStruct(&cookie, password)
		if err != nil {
			debug("failed to read session cookie %s", err)
		}

		saml, err = getSamlSession(&oktaCfg, &cookie)
		if err != nil {
			debug("failed to get session from existing cookie %s", err)
		}
	}

	if saml == nil || saml.raw == "" {
		// final fallback
		sessionToken, err = getSessionFromLogin(&oktaCfg)
		if err != nil {
			fmt.Println("Fatal error getting login session")
			debug("error was %s", err)
			return
		}

		saml, err = getSaml(&oktaCfg, sessionToken)
		if err != nil {
			fmt.Println("Fatal error getting saml")
			debug("error was %s", err)
			return
		}
	}

	mainCreds, mExp, err := assumeFirstRole(acfg, saml)
	if err != nil {
		fmt.Println("Error assuming first role!")
		debug("error was %s", err)
		return
	}

	var finalCreds *credentials.Credentials
	var fExp time.Time
	if !skipSecondRole {
		finalCreds, fExp, err = assumeDestinationRole(acfg, mainCreds)
		if err != nil {
			fmt.Println("Error assuming second role!")
			debug("error was %s", err)
			return
		}
	} else {
		finalCreds = mainCreds
		fExp = mExp
	}

	if opts.ProfileName != "" {
		awsProfile = opts.ProfileName
	}

	// all was good, so let's save credentials...
	err = storeCredsAws(awsProfile, finalCreds)
	if err != nil {
		debug("err storing aws credentials, %s", err)
	}

	err = storeCreds(awsProfile, finalCreds, fExp)
	if err != nil {
		debug("err storing credentials, %s", err)
	}

	debug("Everything looks good; launching your program...")
	err = prepAndLaunch(args, finalCreds)
	if err != nil {
		fmt.Println("Error launching program: ", err)
	}
}

func getSessionFromLogin(oktaCfg *OktaConfig) (string, error) {
	debug := debug.Debug("oktad:getSessionFromLogin")

	user, pass, err := readUserPass()
	if err != nil {
		// if we got an error here, the user bailed on us
		debug("control-c caught in liner, probably")
		return "", errors.New("control-c")
	}

	if user == "" || pass == "" {
		return "", errors.New("Must supply a username and password")
	}

	ores, err := login(oktaCfg, user, pass)
	if err != nil {
		fmt.Println("Error authenticating with Okta! Maybe your username or password are wrong.")
		debug("login err %s", err)
		return "", err
	}

	if ores.Status != "MFA_REQUIRED" {
		return "", errors.New("MFA required to use this tool")
	}

	factor, err := selectTokenFactor(ores)
	
	if err != nil {
		fmt.Println("Error processing okta response!")
		debug("err from extractTokenFactor was %s", err)
		return "", err
	}

 	var sessionToken string
	sessionToken, err = startMfa(ores, factor)
	if err != nil {
		fmt.Println("Error doing MFA!")
		debug("err from startMfa was %s", err)
		return "", err
	}

	return sessionToken, nil
}

func startMfa(ores *OktaLoginResponse, factor *OktaMfaFactor) (sessionToken string, err error) {
	if factor.FactorType == "push" {
		sessionToken, err = startPushMfa(ores, factor)
	} else if factor.FactorType == "token:software:totp" {
		sessionToken, err = startTotpMfa(ores, factor)
	}

	return
}

func startPushMfa(ores *OktaLoginResponse, factor *OktaMfaFactor) (sessionToken string, err error) {
	tries := 0
	debug := debug.Debug("oktad:startPushMfa")

TRYPUSH:
	if tries < 15 {
		sessionToken, err = doMfa(ores, factor, "-")
		if err != nil {
			tries++
			fmt.Println("Push not yet acked, sleeping")
			time.Sleep(time.Duration(2)*time.Second)
			goto TRYPUSH // eat that, Djikstra!
		}
	} else {
		fmt.Println("Error performing MFA auth!")
		debug("error from doMfa was %s", err)
		return "", err
	}

	return sessionToken, nil
}


func startTotpMfa(ores *OktaLoginResponse, factor *OktaMfaFactor) (sessionToken string, err error) {
	tries := 0
	debug := debug.Debug("oktad:startTotpMfa")

TRYMFA:
	mfaToken, err := readMfaToken()
	if err != nil {
		debug("control-c caught in liner, probably")
		return "", err
	}

	if tries < 2 {
		sessionToken, err = doMfa(ores, factor, mfaToken)
		if err != nil {
			tries++
			fmt.Println("Invalid MFA code, please try again.")
			goto TRYMFA // eat that, Djikstra!
		}
	} else {
		fmt.Println("Error performing MFA auth!")
		debug("error from doMfa was %s", err)
		return "", err
	}

	return sessionToken, nil
}

// reads the username and password from the command line
// returns user, then pass, then an error
func readUserPass() (user string, pass string, err error) {
	li := liner.NewLiner()

	// remember to close or weird stuff happens
	defer li.Close()

	li.SetCtrlCAborts(true)
	user, err = li.Prompt("Username: ")
	if err != nil {
		return
	}

	pass, err = li.PasswordPrompt("Password: ")
	if err != nil {
		return
	}

	return
}

// reads and returns an mfa token
func readMfaToken() (string, error) {
	li := liner.NewLiner()
	defer li.Close()
	li.SetCtrlCAborts(true)
	fmt.Println("Your account requires MFA; please enter a token.")
	return li.Prompt("MFA token: ")
}

// pulls the factor we should use out of the response
func selectTokenFactor(ores *OktaLoginResponse) (*OktaMfaFactor, error) {
	debug := debug.Debug("oktad:selectTokenFactor")

	factors := ores.Embedded.Factors
	if len(factors) == 0 {
		return nil, errors.New("MFA factors not present in response")
	}

	if len(factors) == 1 {
		debug("found just a single factor: %s", factors)
		return &factors[0], nil
	}

	li := liner.NewLiner()
	defer li.Close()
	li.SetCtrlCAborts(true)
	

	var mfaFactor OktaMfaFactor
	for i, factor := range factors {
		// need to assert that this is a map
		// since I don't know the structure enough
		// to make a struct for it
		if factor.FactorType == "push" {
			fmt.Printf("[%d] Push notification\n", i)
		}

		if factor.FactorType == "token:software:totp" {
			if factor.Provider == "GOOGLE" {
				fmt.Printf("[%d] Google Authenticator\n",i)
			} else {
				fmt.Printf("[%d] Okta Verify\n",i)
			}
		}
	}

	mfaIdxAnswer, _ := li.Prompt("Select your second factor: ")
	mfaIdx, _ := strconv.Atoi(mfaIdxAnswer)

	mfaFactor = factors[mfaIdx]

	if mfaFactor.Id == "" {
		return nil, wrongMfaError
	}

	return &mfaFactor, nil
}