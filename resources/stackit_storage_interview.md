Part 1: System Design (Approx. 60 minutes)
Scenario
Your goal is to design the process for creating a new storage volume. A customer initiates this process via a single API call. This action requires coordination between multiple services to succeed.

The user story is: "As a customer, I want to request the creation of a new storage volume of a specific size, so I can later attach it to my virtual machine."

Your Task
Provide a high-level design that addresses the following points. You can use diagrams, text, or a combination of both.

API Endpoint Design:
Define the REST endpoint(s) needed to request a new volume and to check the status of that request.
Specify the HTTP method, URL path, and a sample JSON request/response body for each endpoint.
Handling Long-Running Operations:
Creating a volume via the Storage Provisioner can take several minutes. Your API must provide an immediate response to the customer.
Describe the architecture and pattern you would use to handle this asynchronous workflow. How does the customer know when their volume is ready?
Data Consistency and Reliability:
The creation process involves multiple steps: checking the customer's account, charging them, and provisioning the volume. This entire process must be reliable. If the Storage Provisioner fails, for example, the customer should ideally be refunded.
Describe the pattern you would use to ensure consistency across these service calls (e.g., Saga, Choreography vs. Orchestration, Two-Phase Commit). Explain your choice.
Propose a simple database schema for the CloudVolume API's own database to track the state of each volume. At a minimum, what fields would your volumes table need?
Part 2: Coding Implementation (Approx. 60 minutes)
Your Task
Based on your design from Part 1, implement the endpoint for creating a new volume.

Requirements
Language/Framework: Use a language and web framework of your choice (e.g., Python/FastAPI, Node.js/Express, Go/net/http, C#/.NET).
Endpoint:
Implement the POST /volumes endpoint.
Request Body: It should accept a JSON body like: {"customerId": "string", "sizeGB": number}.
Initial Response: On successful initiation, it must immediately return an HTTP 202 Accepted status code and a JSON body that allows the client to track the request, for example: {"taskId": "some-unique-id", "status": "PENDING"}.
Asynchronous Flow:
The endpoint handler should trigger an asynchronous background task. You do not need a real message queue or background worker framework; simulating this with an async function, a goroutine, or a Thread is perfectly acceptable.
Mock Services & Datastore:
Mock a Datastore: Use a simple in-memory object (like a dictionary, map, or list of objects) to act as your database for storing volume state.
Mock Backend Services: Write mock functions to simulate calls to the other services.
checkCustomerStatus(customerId): A function that returns true or false.
chargeCustomer(customerId, amount): A function that simulates a network call and can randomly succeed or fail.
provisionVolume(volumeId, sizeGB): A function that simulates a long-running operation by including a delay (e.g., sleeping for 3-5 seconds) before returning a success or failure status.
Logic:
Upon receiving a request, generate a unique ID for the volume.
Save the initial state (e.g., { "volumeId": "...", "status": "PENDING", ... }) to your in-memory datastore.
Start the asynchronous task that orchestrates the calls to your mocked services.
The task should update the volume's status in the datastore to ACTIVE or FAILED based on the outcome.
Bonus (If you have time)
Implement a GET /volumes/{volumeId} endpoint that retrieves the current status of a specific volume from your in-memory datastore.
