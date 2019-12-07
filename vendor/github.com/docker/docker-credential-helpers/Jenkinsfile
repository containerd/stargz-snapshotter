pipeline {
    agent none
    options {
        checkoutToSubdirectory('src/github.com/docker/docker-credential-helpers')
    }
    stages {
        stage('build') {
            parallel {
                stage('linux') {
                    agent {
                        kubernetes {
                            label 'declarative'
                            containerTemplate {
                                name 'golang'
                                image 'golang:1.12.4'
                                ttyEnabled true
                                command 'cat'
                            }
                        }
                    }
                    environment {
                        GOPATH = pwd()
                        PATH   = "/usr/local/go/bin:${GOPATH}/bin:$PATH"
                    }
                    steps {
                        container('golang') {
                            dir('src/github.com/docker/docker-credential-helpers') {
                                sh 'apt-get update && apt-get install -y libsecret-1-dev pass'
                                sh 'make deps fmt lint test'
                                sh 'make pass secretservice'
                                archiveArtifacts 'bin/docker-credential-*'
                            }
                        }
                    }
                }
                stage('mac') {
                    agent {
                        label 'mac-build && go1.12.4'
                    }
                    environment {
                        PATH   = "/usr/local/go/bin:${GOPATH}/bin:$PATH"
                        GOPATH = pwd()
                    }
                    steps {
                        dir('src/github.com/docker/docker-credential-helpers') {
                            sh 'make deps fmt lint test'
                            sh 'make osxcodesign'
                            archiveArtifacts 'bin/docker-credential-*'
                        }
                    }
                }
                stage('windows') {
                    agent {
                        label 'win-build && go1.12.4'
                    }
                    environment {
                        GOPATH      = pwd()
                        PATH        = "${pwd()}/bin;$PATH"
                        PFX         = credentials('windows-build-pfx-sanitize')
                        PFXPASSWORD = credentials('windows-build-pfx-password')
                    }
                    steps {
                        dir('src/github.com/docker/docker-credential-helpers') {
                            sh 'echo ${PFX} | base64 -d > pfx'

                            sh 'make deps fmt lint test'
                            sh 'make wincred'
                            bat """ "C:\\Program Files (x86)\\Windows Kits\\10\\bin\\x86\\signtool.exe" sign /fd SHA256 /a /f pfx /p ${PFXPASSWORD} /d Docker /du https://www.docker.com /t http://timestamp.verisign.com/scripts/timestamp.dll bin\\docker-credential-wincred.exe """
                            archiveArtifacts 'bin/docker-credential-*'
                        }
                    }
                    post {
                        always {
                            sh 'rm -f pfx'
                        }
                    }
                }
            }
        }
    }
}
