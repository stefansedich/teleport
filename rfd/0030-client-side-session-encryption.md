---
authors: Trent Clarke (trent@goteleport.com)
state: draft
---

# Local Encryption of Session Data on S3

## What

Provide encrypted-at-rest session recordings that do not rely on AWS' server-side encryption.

## Why

When Teleport uploads a session recording to S3 it is (optionally) encrypted at rest by S3 server-side encryption. 

For whatever reason, some clients do not want to share encryption keys with AWS. To this end, S3 offers the option of encrypting the data on the client side during the upload process, using client-managed keys. We can use this service to provide at-rest encryption on S3, without sharing keys with Amazon.

## Details

### Background

There are two keys involved in storing an object on S3 with client-side encryption

 1. The master key, Key Encryption Key, or KEK. This is the ultimate secret that is required to decrypt an object 
 2. The session key, Content Encryption Key, or CEK. This is a one-off key used to encrypt the object content. This key os in turn encrypted using the KEK and embedded into the stored object.

When an object is downloaded the encrypted CEK is extracted from the object and decrypted using the master key, and then the CEK is then used to decode the actual content.

### Key Management

AWS provides two options for managing the master encryption key:
 1. The Amazon Key Management Service, where the master key is store remotely and retrieved from KMS prior to upload, and 
 2. A locally-managed key that never leaves the client side.

Given that one the driving goals of using client-side encryption is to avoid having the master encryption keys leave the client's possession, this document will concern itself with option 2.

Unfortunately, the AWS SDK for Go (does not support client-side encryption using purely local keys)[https://github.com/aws/aws-sdk-go/issues/1241]. It does, however, provide the appropriate hooks so we can make one ourselves. For more details, see the "Design & Implementation" section below.

### Cypher selection

The AWS client code supports both symmetric and asymmetric encryption.

Using an asymmetric cypher would be more secure, in that the client would not have to distribute a private decryption key to the nodes doing the encryption. Using an asymmetric cypher would, however, totally break the session playback system. Teleport would have no way of decrypting the sessions in order to play them back.

For that reason, and for general simplicity, we will proceed under the assumption that only symmetric cyphers will be supported. The default symmetric algorithm is AES256.

### S3 Manager

Client side encryption requires the use of the `EncryptionClient` type to act as the upload and download interface.

The current session uploader uses the `s3manager` type to manage concurrent uploads, downloads, chunking, retries, etc. The `s3manager` creates its own upload and download clients internally, and does not appear to allow us to substitute the `EncryptionClient`.

This means that the implementation for the encrypted uploader and downloader will have to be more involved than the current handlers that use the `s3manager`.

## Design and Implementation

### Configuration

The user may specify 256-bit encryption key in the teleport config file, in the `teleport/storage` block. The value may be either a hex string, or a path to a file containing a hex encoded key.

```yaml
teleport:
  storage:
    audit_sessions_uri: s3://teleport/recordings?region=ap-southeast-2
    audit_sessions_local_key: "020a870107feb729b6795f6248a592e5332f71a2981455f91770d0c48096bca9"
```
    
If the key is supplied, Teleport will use client-side encryption, with the supplied key acting as the master key. 

If no key is supplied Teleport will use the defaults foe the target bucket.

### Key Wrapping Algorithm

The AWS SDK uses interfaces defined in the `s3crypto` package to define key wrapping algorithms. If we provide a compatible Key wrapping and unwrapping mechanism, we _should_ be able to use the existing SD machinery to create and retrieve client-side-encrypted object in a manner compatible with other clients.

The Java SDK uses the well-defined (RFC3394 AESWrap)[https://datatracker.ietf.org/doc/html/rfc3394] key wrapping algorithm. (Several)[https://pkg.go.dev/github.com/chiefbrain/ios/crypto/aeswrap] (implementations)[https://pkg.go.dev/github.com/dunhamsteve/ios/crypto/aeswrap] (of RFC3394)[https://pkg.go.dev/github.com/gwatts/ios/crypto/aeswrap] in Go already exist.

Integrating the key wrapping algorithm would require
 1. Selecting an implementation (or implementing our own, but probably not) of RFC3394
 2. Wrapping the RFC in a type 
   * that knows the master key, and 
   * implements the `s3crypto.ContentCipherBuilder` and `s3crypto.CipherDataDecrypter` interfaces
 3. Registering the `CipherDataDecrypter` in an `s3crypto.Registry` that the S3 SDK can use it to decrypt objects on download

### Upload and Download


