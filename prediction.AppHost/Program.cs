

var builder = DistributedApplication.CreateBuilder(args);

builder.AddProject<Projects.prediction_Server>("prediction-server");

builder.AddProject<Projects.AccessControlEmulator>("acess-emulator");
builder.AddProject<Projects.prediction_client>("Client");

builder.Build().Run();
