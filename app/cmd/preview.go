package cmd

import (
	"archive/zip"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/briandowns/spinner"
	pb "github.com/cheggaaa/pb/v3"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/gSchool/glearn-cli/api/learn"
	proxyReader "github.com/gSchool/glearn-cli/app/proxy_reader"
)

// tmpFile is used throughout as the temporary zip file target location.
const tmpFile string = "preview-curriculum.zip"

// previewCmd is executed when the `learn preview` command is used. Preview's concerns:
// 1. Compress directory/file into target location.
// 2. Defer cleaning up the file after command is finished.
// 3. Create a checksum for the zip file.
// 4. Upload the zip file to s3.
// 5. Notify learn that new content is available for building.
// 6. Handle progress bar for s3 upload.
var previewCmd = &cobra.Command{
	Use:   "preview [file_path]",
	Short: "Uploads content and builds a preview.",
	Long: `
		The preview command takes a path to either a directory or a single file and
		uploads the content to Learn through the Learn API. Learn will build the preview
		and return/open the preview URL when it is complete.
	`,
	Args: cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		if viper.Get("api_token") == "" || viper.Get("api_token") == nil {
			previewCmdError("Please set your API token first with `learn set --api_token=value`")
		}

		// Takes one argument which is the filepath to the directory you want zipped/previewed
		if len(args) != 1 {
			previewCmdError("Usage: `learn preview` takes one argument")
			return
		}

		// Detect config file
		doesConfigExistOrCreate(args[0], UnitsDirectory)

		// Compress directory, output -> tmpFile
		err := compressDirectory(args[0], tmpFile)
		if err != nil {
			previewCmdError(fmt.Sprintf("Error compressing directory %s: %v", args[0], err))
			return
		}

		// Removes artifacts on user's machine
		defer cleanUpFiles()

		// Open file so we can get a checksum as well as send to s3
		f, err := os.Open(tmpFile)
		if err != nil {
			previewCmdError(fmt.Sprintf("Failed to open file %q, %v", tmpFile, err))
			return
		}
		defer f.Close()

		// Create checksum of files in directory
		checksum, err := createChecksumFromZip(f)
		if err != nil {
			previewCmdError(fmt.Sprintf("Failed to create checksum for compressed file. Err: %v", err))
			return
		}

		// Send compressed zip file to s3
		bucketKey, err := uploadToS3(f, checksum)
		if err != nil {
			previewCmdError(fmt.Sprintf("Failed to upload zip file to s3. Err: %v", err))
			return
		}

		// Get os.FileInfo from call to os.Stat so we can see if it is a single file or directory
		fileInfo, err := os.Stat(args[0])
		if err != nil {
			previewCmdError(fmt.Sprintf("Failed to get stats on file. Err: %v", err))
			return
		}
		isDirectory := fileInfo.IsDir()

		fmt.Println("\nPlease wait while Learn builds your preview...")

		// Start a processing spinner that runs until Learn is finsihed building the preview
		s := spinner.New(spinner.CharSets[32], 100*time.Millisecond)
		s.Color("green")
		s.Start()

		// Let Learn know there is new preview content on s3, where it is, and to build it
		res, err := learn.API.BuildReleaseFromS3(bucketKey, isDirectory)
		if err != nil {
			previewCmdError(fmt.Sprintf("Failed to notify learn of new preview content. Err: %v", err))
			return
		}

		// If content is a directory, rewrite the res from polling for build response. Directories
		// can take much longer to build, however single files build instantly so we do not need to
		// poll for them because the call to BuildReleaseFromS3 will get a preview_url right away
		if isDirectory {
			var attempts uint8 = 20
			res, err = learn.API.PollForBuildResponse(res.ReleaseID, &attempts)
			if err != nil {
				previewCmdError(fmt.Sprintf("Failed to poll Learn for your new preview build. Err: %v", err))
				return
			}
		}

		// Set final message for dislpay
		s.FinalMSG = fmt.Sprintf("Sucessfully uploaded your preview! You can find your content at: %s\n", res.PreviewURL)

		// Stop the processing spinner
		s.Stop()

		exec.Command("bash", "-c", fmt.Sprintf("open %s", res.PreviewURL)).Output()
	},
}

// previewCmdError is a small wrapper for all errors within the preview command. It ensures
// artifacts are cleaned up with a call to cleanUpFiles
func previewCmdError(msg string) {
	fmt.Println(msg)
	cleanUpFiles()
	os.Exit(1)
}

// uploadToS3 takes a file and it's checksum and uploads it to s3 in the appropriate bucket/key
func uploadToS3(file *os.File, checksum string) (string, error) {
	// Retrieve the application credentials from AWS
	creds, err := learn.API.RetrieveS3Credentials()
	if err != nil {
		return "", fmt.Errorf(
			"Could not retrieve credentials from Learn. Please ensure you have the right API key in your ~/.glearn-config.yaml %s",
			file.Name(),
		)
	}

	// Set up an AWS session with the user's credentials
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String("us-west-2"),
		Credentials: credentials.NewStaticCredentials(
			creds.AccessKeyID,
			creds.SecretAccessKey,
			"",
		),
	})

	// Create new uploader and specify buffer size (in bytes) to use when buffering
	// data into chunks and sending them as parts to S3 and clean up on error
	uploader := s3manager.NewUploader(sess, func(u *s3manager.Uploader) {
		u.PartSize = 5 * 1024 * 1024 // 5,242,880 bytes or 5.24288 Mb which is the default minimum here
		u.LeavePartsOnError = false  // If an error occurs during upload to s3, clean up & don't leave partial upload there
	})

	// Generate the bucket key using the key prefix, checksum, and tmpFile name
	bucketKey := fmt.Sprintf("%s/%s-%s", creds.KeyPrefix, checksum, tmpFile)

	// Obtain FileInfo so we can look at length in bytes
	fileStats, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("Could not obtain file stats for %s", file.Name())
	}

	// Create and start a new progress bar with a fixed width
	bar := pb.Full.Start64(fileStats.Size()).SetWidth(100)

	// Create a ProxyReader and attach the file and progress bar
	pr := proxyReader.New(file, bar)

	fmt.Println("Uploading assets to Learn...")

	// Upload compressed zip file to s3
	_, err = uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(creds.BucketName),
		Key:    aws.String(bucketKey),
		Body:   pr, // As our file is read and uploaded, our proxy reader will update/render the progress bar
	})
	if err != nil {
		return "", fmt.Errorf("Error uploading assets to s3: %v", err)
	}

	bar.Finish()

	return bucketKey, nil
}

// createChecksumFromZip takes a pointer to a file and creates a sha256 checksum
// of the content. We use this for naming the s3 bucket key so that we don't write
// duplicates to s3. The call to io.Copy actually consumes the read position of
// the file to EOF so we call file.Seek and set the read position back to the
// beginning of the file
func createChecksumFromZip(file *os.File) (string, error) {
	// Create a sha256 hash of the curriculum directory
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	// Make the hash URL safe with base64
	checksum := base64.URLEncoding.EncodeToString(hash.Sum(nil))

	// The io.Copy call for producing the hash consumed the read position of the
	// file (file now at EOF). Need to reset to beginning for sending to s3
	_, err := file.Seek(0, io.SeekStart)
	if err != nil {
		log.Fatal(err)
		os.Exit(1)
		return "", err
	}

	return checksum, nil
}

// cleanUpFiles removes the tmp zipfile that was created for uploading to s3. We
// wouldn't want to leave artifacts on user's machines
func cleanUpFiles() {
	err := os.Remove(tmpFile)
	if err != nil {
		fmt.Println("Sorry, we had trouble cleaning up the zip file created for curriculum preview")
	}
}

// compressDirectory takes a source file path (where the content you want zipped lives)
// and a target file path (where to put the zip file) and recursively compresses the source.
// Source can either be a directory or a single file
func compressDirectory(source, target string) error {
	// Create file with target name and defer its closing
	zipfile, err := os.Create(target)
	if err != nil {
		return err
	}
	defer zipfile.Close()

	// Create a new zip writer and pass our zipfile in
	archive := zip.NewWriter(zipfile)
	defer archive.Close()

	// Get os.FileInfo about our source
	info, err := os.Stat(source)
	if err != nil {
		return nil
	}

	// Check to see if the provided source file is a directory and set baseDir if so
	var baseDir string
	if info.IsDir() {
		baseDir = filepath.Base(source)
	}

	// Walk the whole filepath
	filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Creates a partially-populated FileHeader from an os.FileInfo
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		// Check if baseDir has been set (from the IsDir check) and if it has not been
		// set, update the header.Name to reflect the correct path
		if baseDir != "" {
			header.Name = filepath.Join(baseDir, strings.TrimPrefix(path, source))
		}

		// Check if the file we are iterating is a directory and update the header.Name
		// or the header.Method appropriately
		if info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}

		//  Add a file to the zip archive using the provided FileHeader for the file metadata
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}

		// Return nil if at this point if info is a directory
		if info.IsDir() {
			return nil
		}

		// If it was not a directory, we open the file and copy it into the archive writer
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(writer, file)

		return err
	})

	return err
}

// Check whether or nor a config file exists and if it does not we are going to attempt to create one
func doesConfigExistOrCreate(target, unitsDir string) {
	// Configs can be `yaml` or `yml`
	configYamlPath := ""
	if strings.HasSuffix(target, "/") {
		configYamlPath = target + "config.yaml"
	} else {
		configYamlPath = target + "/config.yaml"
	}

	configYmlPath := ""
	if strings.HasSuffix(target, "/") {
		configYmlPath = target + "config.yml"
	} else {
		configYmlPath = target + "/config.yml"
	}

	_, yamlExists := os.Stat(configYamlPath)
	if yamlExists == nil { // Yaml exists
		log.Printf("WARNING: There is a config present and one will not be generated.")
	} else if os.IsNotExist(yamlExists) {

		_, ymlExists := os.Stat(configYmlPath)
		if ymlExists == nil { // Yml exists
			log.Printf("WARNING: There is a config present and one will not be generated.")
		} else if os.IsNotExist(ymlExists) {
			// Neither exists so we are going to create one
			log.Printf("WARNING: No config was found, one will be generated for you.")
			createAutoConfig(target, unitsDir)
		}
	}
}

// Creates a config file based on three things:
// 1. Did you give us a units directory?
// 2. Do you have a units directory?
// 3. We will use the root (target) directory
func createAutoConfig(target, requestedUnitsDir string) {
	blockRoot := ""
	// Make sure we have an ending slash on the root dir
	if strings.HasSuffix(target, "/") {
		blockRoot = target
	} else {
		blockRoot = target + "/"
	}

	// The config file location that we will be creating
	autoConfigYamlPath := blockRoot + "autoconfig.yaml"

	// Remove the existing one if its around
	_, autoYamlExists := os.Stat(autoConfigYamlPath)
	if autoYamlExists == nil {
		os.Remove(autoConfigYamlPath)
	}

	// Create the config file
	var configFile, err = os.Create(autoConfigYamlPath)
	if err != nil {
		fmt.Println(err)
		return
	}

	// If no unitsDir was passed in, create a Units directory string
	unitsDir := ""
	unitsDirName := ""
	if requestedUnitsDir == "" {
		unitsDir = blockRoot + "units"
		unitsDirName = "Unit 1"
	} else {
		unitsDir = blockRoot + requestedUnitsDir
		unitsDirName = requestedUnitsDir
	}

	unitToContentFileMap := map[string][]string{}

	// Check to see if units directory exists
	_, unitsDirExists := os.Stat(unitsDir)
	whereToLookForUnits := blockRoot
	if unitsDirExists == nil {
		whereToLookForUnits = unitsDir

		allItems, _ := ioutil.ReadDir(whereToLookForUnits)
		for _, info := range allItems {
			if info.Mode().IsRegular() && strings.HasSuffix(info.Name(), ".md") {
				unitToContentFileMap[unitsDirName] = append(unitToContentFileMap[unitsDirName], info.Name())
			}
		}
	}

	// Find all the directories in the block
	directories := []string{}
	allDirs, _ := ioutil.ReadDir(whereToLookForUnits)
	for _, info := range allDirs {
		if info.IsDir() {
			directories = append(directories, info.Name())
		}
	}

	if len(directories) > 0 {
		for _, dirName := range directories {
			nestedFolder := ""
			if dirName != ".git" {
				if strings.HasSuffix(whereToLookForUnits, "/") {
					nestedFolder = whereToLookForUnits + dirName
				} else {
					nestedFolder = whereToLookForUnits + "/" + dirName
				}

				err = filepath.Walk(nestedFolder,
					func(path string, info os.FileInfo, err error) error {
						if err != nil {
							return err
						}

						localPath := path[len(blockRoot):len(path)]
						if strings.HasSuffix(path, ".md") {
							unitToContentFileMap[dirName] = append(unitToContentFileMap[dirName], localPath)
						}
						return nil
					})
			}
		}
	}

	configFile.WriteString("---\n")
	configFile.WriteString("Standards:\n")
	configFile.WriteString("  -\n")
	for unit, paths := range unitToContentFileMap {
		configFile.WriteString("    Title: " + formattedName(unit) + "\n")
		var unitUID = []byte(formattedName(unit))
		var md5unitUID = md5.Sum(unitUID)
		configFile.WriteString("    Descripion: " + formattedName(unit) + "\n")
		configFile.WriteString("    UID: " + hex.EncodeToString(md5unitUID[:]) + "\n")
		configFile.WriteString("    SuccessCriteria:\n")
		configFile.WriteString("      - success criteria\n")
		configFile.WriteString("    ContentFiles:\n")
		for _, path := range paths {
			if path != "README.md" {
				configFile.WriteString("      -\n")
				configFile.WriteString("        Type: Lesson\n")
				var cfUID = []byte(formattedName(unit) + path)
				var md5cfUID = md5.Sum(cfUID)
				configFile.WriteString("        UID: " + hex.EncodeToString(md5cfUID[:]) + "\n")
				configFile.WriteString("        Path: /" + path + "\n")
			}
		}
	}
	if err != nil {
		fmt.Println(err)
		return
	}
	err = configFile.Sync()
	defer configFile.Close()
	if err != nil {
		fmt.Println(err)
		return
	}
}

func formattedName(name string) string {
	parts := strings.Split(name, "/")
	parts = strings.Split(parts[len(parts)-1], ".")
	a := regexp.MustCompile(`\-`)
	parts = a.Split(parts[0], -1)
	a = regexp.MustCompile(`\_`)
	parts = a.Split(strings.Join(parts, " "), -1)
	formattedName := ""
	for _, piece := range parts {
		formattedName = formattedName + " " + strings.Title(piece)
	}

	return strings.TrimSpace(formattedName)
}
